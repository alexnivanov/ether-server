package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func mustUnmarshal(t *testing.T, data json.RawMessage, v any) {
	t.Helper()
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("unmarshal %s: %v", data, err)
	}
}

// newTestServer поднимает REST + /ws на одном сервере, как main.go, поверх
// свежего Store и StubGeocoder. Эти тесты не ходят в /auth/telegram (сессии
// заводятся напрямую через Store), поэтому TelegramAuth получает фиктивный JWKS
// URL — он лениво тянется только при первом входе (см. TestAuthTelegram).
func newTestServer(t *testing.T) (*httptest.Server, *Store) {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "e2e.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	hub := NewHub()
	go hub.Run()
	tg := NewTelegramAuth("test-client", "http://127.0.0.1:0/jwks")

	mux := http.NewServeMux()
	registerREST(mux, store, tg)
	mux.HandleFunc("/ws", wsHandler(hub, StubGeocoder{}, store))

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, store
}

func restPost(t *testing.T, url string, body any) (*http.Response, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatalf("decode response from %s: %v", url, err)
	}
	return resp, m
}

// Сквозная проверка REST-эндпоинтов, вынесенных из WS (resume/accept_rules/
// history): мусорный токен — 401 bad_session, настоящий — данные аккаунта,
// принятие правил персистентно и видно в следующем resume, история отдаёт
// то, что было опубликовано по WS.
func TestRESTSessionFlow(t *testing.T) {
	srv, store := newTestServer(t)

	if _, err := store.SaveUser(User{TgID: 7, TgUsername: "alex", FullName: "alex"}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	token, err := store.NewSession(7)
	if err != nil {
		t.Fatalf("seed session: %v", err)
	}

	// мусорный токен не пускает
	resp, body := restPost(t, srv.URL+"/session/resume", ResumeData{Token: "garbage"})
	if resp.StatusCode != http.StatusUnauthorized || body["code"] != "bad_session" {
		t.Fatalf("resume(garbage) = %d %v, want 401 bad_session", resp.StatusCode, body)
	}

	// настоящий токен отдаёт личность аккаунта
	resp, body = restPost(t, srv.URL+"/session/resume", ResumeData{Token: token})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("resume(token) = %d %v, want 200", resp.StatusCode, body)
	}
	var authed AuthedData
	raw, _ := json.Marshal(body)
	mustUnmarshal(t, raw, &authed)
	if authed.User.ID != 7 || authed.User.Name != "alex" || authed.Token != "" {
		t.Fatalf("resume: %+v, want token пустой (клиент его и так знает)", authed)
	}
	if authed.RulesAccepted {
		t.Fatalf("resume: rules_accepted = true до accept_rules")
	}

	// без токена accept_rules недоступен
	resp, body = restPost(t, srv.URL+"/rules/accept", AcceptRulesData{})
	if resp.StatusCode != http.StatusBadRequest || body["code"] != "not_authed" {
		t.Fatalf("accept_rules() = %d %v, want 400 not_authed", resp.StatusCode, body)
	}

	// принятие правил персистентно и видно в следующем resume (аккаунт, не устройство)
	resp, body = restPost(t, srv.URL+"/rules/accept", AcceptRulesData{Token: token})
	if resp.StatusCode != http.StatusOK || body["rules_accepted"] != true {
		t.Fatalf("accept_rules(token) = %d %v, want 200 rules_accepted=true", resp.StatusCode, body)
	}
	_, body = restPost(t, srv.URL+"/session/resume", ResumeData{Token: token})
	if body["rules_accepted"] != true {
		t.Fatalf("resume после accept_rules: %v, want rules_accepted=true", body)
	}

	// история пуста, пока никто не публиковал
	histResp, err := http.Get(srv.URL + "/history?channel=RU")
	if err != nil {
		t.Fatalf("GET history: %v", err)
	}
	defer histResp.Body.Close()
	var h HistoryData
	if err := json.NewDecoder(histResp.Body).Decode(&h); err != nil {
		t.Fatalf("decode history: %v", err)
	}
	if len(h.Messages) != 0 {
		t.Fatalf("history пустого канала: %+v", h)
	}

	// публикуем через WS (с тем же токеном — см. TestWebSocketTokenAuth) и
	// проверяем, что REST history видит результат
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws?token=" + token
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial ws: %v", err)
	}
	defer ws.Close()
	ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	if err := ws.WriteJSON(envelope(TypeLocate, LocateData{Lat: 55.76, Lng: 37.61})); err != nil {
		t.Fatalf("write locate: %v", err)
	}
	var located Envelope
	if err := ws.ReadJSON(&located); err != nil {
		t.Fatalf("read located: %v", err)
	}
	if err := ws.WriteJSON(envelope(TypePublish, PublishData{Channel: "RU", Text: "привет"})); err != nil {
		t.Fatalf("write publish: %v", err)
	}
	var msgEnv Envelope
	if err := ws.ReadJSON(&msgEnv); err != nil {
		t.Fatalf("read message: %v", err)
	}

	histResp2, err := http.Get(srv.URL + "/history?channel=RU")
	if err != nil {
		t.Fatalf("GET history 2: %v", err)
	}
	defer histResp2.Body.Close()
	var h2 HistoryData
	if err := json.NewDecoder(histResp2.Body).Decode(&h2); err != nil {
		t.Fatalf("decode history 2: %v", err)
	}
	if len(h2.Messages) != 1 || h2.Messages[0].Text != "привет" || h2.Messages[0].Sender != "alex" {
		t.Fatalf("history после publish: %+v", h2)
	}

	// logout отзывает сессию: тот же токен после него в resume — bad_session
	resp, body = restPost(t, srv.URL+"/session/logout", LogoutData{Token: token})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("logout(token) = %d %v, want 200", resp.StatusCode, body)
	}
	resp, body = restPost(t, srv.URL+"/session/resume", ResumeData{Token: token})
	if resp.StatusCode != http.StatusUnauthorized || body["code"] != "bad_session" {
		t.Fatalf("resume после logout = %d %v, want 401 bad_session", resp.StatusCode, body)
	}
	// повторный logout того же токена — тоже 200 (идемпотентность)
	resp, _ = restPost(t, srv.URL+"/session/logout", LogoutData{Token: token})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("повторный logout = %d, want 200 (идемпотентно)", resp.StatusCode)
	}
}

// Проверяет аутентификацию WS в момент апгрейда через ?token=: мусорный токен
// отбивается ещё до открытия сокета (401, апгрейд не происходит), настоящий —
// сокет сразу authed без единого кадра, анонимное подключение (без токена)
// может locate, но не publish.
func TestWebSocketTokenAuth(t *testing.T) {
	srv, store := newTestServer(t)

	if _, err := store.SaveUser(User{TgID: 9, TgUsername: "bob", FullName: "bob"}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	token, err := store.NewSession(9)
	if err != nil {
		t.Fatalf("seed session: %v", err)
	}
	wsBase := "ws" + strings.TrimPrefix(srv.URL, "http")

	// мусорный токен — апгрейд не проходит вовсе
	_, resp, err := websocket.DefaultDialer.Dial(wsBase+"/ws?token=garbage", nil)
	if err == nil {
		t.Fatalf("dial(garbage token): ожидалась ошибка апгрейда")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("dial(garbage token): status = %v, want 401", resp)
	}

	// настоящий токен — сокет уже authed, publish проходит без единого кадра входа
	ws, _, err := websocket.DefaultDialer.Dial(wsBase+fmt.Sprintf("/ws?token=%s", token), nil)
	if err != nil {
		t.Fatalf("dial(valid token): %v", err)
	}
	defer ws.Close()
	ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	// подписка на канал — иначе broadcast некому доставить, включая отправителя
	if err := ws.WriteJSON(envelope(TypeLocate, LocateData{Lat: 55.76, Lng: 37.61})); err != nil {
		t.Fatalf("write locate: %v", err)
	}
	var env Envelope
	if err := ws.ReadJSON(&env); err != nil {
		t.Fatalf("read located: %v", err)
	}
	if err := ws.WriteJSON(envelope(TypePublish, PublishData{Channel: "RU", Text: "hi"})); err != nil {
		t.Fatalf("write publish: %v", err)
	}
	if err := ws.ReadJSON(&env); err != nil {
		t.Fatalf("read after publish: %v", err)
	}
	if env.Type != TypeMessage {
		t.Fatalf("got %s %s, want message (сокет должен быть authed сразу по токену)", env.Type, env.Data)
	}

	// анонимное подключение: locate работает, publish — нет
	anon, _, err := websocket.DefaultDialer.Dial(wsBase+"/ws", nil)
	if err != nil {
		t.Fatalf("dial(anon): %v", err)
	}
	defer anon.Close()
	anon.SetReadDeadline(time.Now().Add(5 * time.Second))
	if err := anon.WriteJSON(envelope(TypePublish, PublishData{Channel: "RU", Text: "hi"})); err != nil {
		t.Fatalf("write publish anon: %v", err)
	}
	if err := anon.ReadJSON(&env); err != nil {
		t.Fatalf("read after publish anon: %v", err)
	}
	var e ErrorData
	mustUnmarshal(t, env.Data, &e)
	if env.Type != TypeError || e.Code != "not_authed" {
		t.Fatalf("anon publish: got %s %+v, want error not_authed", env.Type, e)
	}
}
