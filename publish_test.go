package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// publishSession — аккаунт и живая сессия для POST /messages.
func publishSession(t *testing.T, store *Store) (int64, string) {
	t.Helper()
	userID := mkTgUser(t, store, "777", "alex", "Alex")
	token, err := store.NewSession(userID)
	if err != nil {
		t.Fatalf("сессия: %v", err)
	}
	return userID, token
}

// postMessage — POST /messages, возвращает статус и разобранное тело.
func postMessage(t *testing.T, url string, req PublishRequest) (*http.Response, map[string]any) {
	t.Helper()
	return restPost(t, url+"/messages", req)
}

// TestPublishREST — обычная отправка: сообщение уходит в ответ и ложится в
// историю.
func TestPublishREST(t *testing.T) {
	srv, store := newTestServer(t)
	userID, token := publishSession(t, store)

	resp, body := postMessage(t, srv.URL, PublishRequest{
		Token: token, Channel: "RU-MOW", Text: "привет",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%v)", resp.StatusCode, body)
	}
	m, ok := body["message"].(map[string]any)
	if !ok {
		t.Fatalf("в ответе нет message: %v", body)
	}
	if m["text"] != "привет" || m["channel"] != "RU-MOW" {
		t.Errorf("сообщение = %v", m)
	}
	if id, _ := m["id"].(float64); id <= 0 {
		t.Errorf("id = %v, want > 0 — по нему клиент отсеивает эхо из сокета", m["id"])
	}
	if sender, _ := m["sender"].(string); sender != "Alex" {
		t.Errorf("sender = %q, want Alex — имя берётся из сессии", sender)
	}

	// в историю сообщение попало ровно одно
	var text string
	if err := store.db.QueryRow(
		`SELECT text FROM messages WHERE user_id = ?`, userID).Scan(&text); err != nil {
		t.Fatalf("чтение истории: %v", err)
	}
	if text != "привет" {
		t.Errorf("в истории %q, want привет", text)
	}
}

// TestPublishRESTIdempotent — повтор с тем же client_msg_id не создаёт второе
// сообщение. Ради этого отправка и уехала с WS: клиент, не получивший ответ,
// обязан иметь право повторить запрос.
func TestPublishRESTIdempotent(t *testing.T) {
	srv, store := newTestServer(t)
	userID, token := publishSession(t, store)
	req := PublishRequest{
		Token: token, Channel: "RU-MOW", Text: "привет", ClientMsgID: "abc-123",
	}

	_, first := postMessage(t, srv.URL, req)
	resp, second := postMessage(t, srv.URL, req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("повтор: status = %d, want 200 — это успех, а не ошибка", resp.StatusCode)
	}

	firstID := first["message"].(map[string]any)["id"]
	secondID := second["message"].(map[string]any)["id"]
	if firstID != secondID {
		t.Errorf("id = %v и %v — повтор вернул другое сообщение", firstID, secondID)
	}
	var n int
	if err := store.db.QueryRow(
		`SELECT COUNT(*) FROM messages WHERE user_id = ?`, userID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("сообщений в истории %d, want 1", n)
	}

	// другой client_msg_id — уже другое сообщение
	req.ClientMsgID = "abc-124"
	postMessage(t, srv.URL, req)
	if err := store.db.QueryRow(
		`SELECT COUNT(*) FROM messages WHERE user_id = ?`, userID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Errorf("сообщений в истории %d, want 2 — id отправки другой", n)
	}
}

// TestPublishRESTIdempotentPerUser — client_msg_id уникален В ПРЕДЕЛАХ автора:
// его придумывает клиент, и совпадение у двух разных людей вероятно (у нас это
// uuid, но полагаться на это нельзя). Чужая отправка не должна подменяться.
func TestPublishRESTIdempotentPerUser(t *testing.T) {
	srv, store := newTestServer(t)
	_, tokenA := publishSession(t, store)
	userB := mkTgUser(t, store, "888", "bob", "Bob")
	tokenB, err := store.NewSession(userB)
	if err != nil {
		t.Fatalf("сессия B: %v", err)
	}

	_, a := postMessage(t, srv.URL, PublishRequest{
		Token: tokenA, Channel: "RU-MOW", Text: "от Alex", ClientMsgID: "same",
	})
	_, b := postMessage(t, srv.URL, PublishRequest{
		Token: tokenB, Channel: "RU-MOW", Text: "от Bob", ClientMsgID: "same",
	})
	if a["message"].(map[string]any)["id"] == b["message"].(map[string]any)["id"] {
		t.Error("сообщения слиплись: client_msg_id считается уникальным на всех")
	}
	if got := b["message"].(map[string]any)["text"]; got != "от Bob" {
		t.Errorf("текст = %v, want «от Bob»", got)
	}
}

// TestPublishRESTRejects — отказы в формате, который уже понимает клиент.
func TestPublishRESTRejects(t *testing.T) {
	srv, store := newTestServer(t)
	_, token := publishSession(t, store)

	long := make([]byte, maxMessageLen+1)
	for i := range long {
		long[i] = 'x'
	}
	cases := []struct {
		name string
		req  PublishRequest
		want int
		code string
	}{
		{"без токена", PublishRequest{Channel: "RU", Text: "x"}, 400, "bad_data"},
		{"мёртвая сессия", PublishRequest{Token: "нет такой", Channel: "RU", Text: "x"}, 401, "bad_session"},
		{"без канала", PublishRequest{Token: token, Text: "x"}, 400, "bad_data"},
		{"пустой текст", PublishRequest{Token: token, Channel: "RU"}, 400, "bad_data"},
		{"текст длиннее предела", PublishRequest{Token: token, Channel: "RU", Text: string(long)}, 400, "bad_data"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp, body := postMessage(t, srv.URL, c.req)
			if resp.StatusCode != c.want {
				t.Errorf("status = %d, want %d (%v)", resp.StatusCode, c.want, body)
			}
			if body["code"] != c.code {
				t.Errorf("code = %v, want %q", body["code"], c.code)
			}
		})
	}
}

// TestPublishRESTMuted — временный бан (мьют) закрывает отправку и через REST.
// Смысл общей функции публикации именно в этом: через один транспорт не должно
// быть можно то, что запрещено через другой.
//
// Бан именно временный: постоянный отзывает сессии (см. BanEscalate), и REST
// отобьёт такой запрос раньше — на проверке сессии, кодом bad_session. Мьют же
// оставляет сессию живой: читать можно, отправлять нет.
func TestPublishRESTMuted(t *testing.T) {
	srv, store := newTestServer(t)
	userID, token := publishSession(t, store)
	if _, _, err := store.BanTemporary(userID, "спам"); err != nil {
		t.Fatalf("мьют: %v", err)
	}

	resp, body := postMessage(t, srv.URL, PublishRequest{
		Token: token, Channel: "RU", Text: "всё равно напишу",
	})
	if resp.StatusCode != http.StatusForbidden || body["code"] != "banned" {
		t.Errorf("status/code = %d/%v, want 403/banned", resp.StatusCode, body["code"])
	}
}

// TestPublishRESTBannedSession — постоянный бан отзывает сессии, поэтому до
// проверки бана дело не доходит: клиент получает bad_session и уходит на
// онбординг, где вход ему закроет уже /auth.
func TestPublishRESTBannedSession(t *testing.T) {
	srv, store := newTestServer(t)
	userID, token := publishSession(t, store)
	if _, err := store.BanPermanent(userID, "спам"); err != nil {
		t.Fatalf("бан: %v", err)
	}

	resp, body := postMessage(t, srv.URL, PublishRequest{
		Token: token, Channel: "RU", Text: "всё равно напишу",
	})
	if resp.StatusCode != http.StatusUnauthorized || body["code"] != "bad_session" {
		t.Errorf("status/code = %d/%v, want 401/bad_session", resp.StatusCode, body["code"])
	}
}

// TestPublishRESTTooFast — лимит частоты общий с WS: у REST он тот же бакет, и
// отказ несёт Retry-After.
func TestPublishRESTTooFast(t *testing.T) {
	_, store := newTestServer(t)
	_, token := publishSession(t, store)
	// у тестового сервера лимитера нет, поэтому берём эндпоинт со своим
	mux := http.NewServeMux()
	pub := &publisher{store: store, hub: NewHub(), limiter: NewRateLimiter()}
	go pub.hub.Run()
	mux.HandleFunc("POST /messages", handlePublish(store, pub))
	limitedSrv := httptest.NewServer(mux)
	defer limitedSrv.Close()
	limited := limitedSrv.URL

	// свежий аккаунт: тир уже базового, но сколько именно — дело ratelimit.go.
	// Шлём заведомо больше любого разумного всплеска и ждём отказ.
	var status int
	var body map[string]any
	var resp *http.Response
	for i := 0; i < 20; i++ {
		resp, body = postMessage(t, limited, PublishRequest{
			Token: token, Channel: "RU", Text: "спам",
		})
		if resp.StatusCode != http.StatusOK {
			status = resp.StatusCode
			break
		}
	}
	if status != http.StatusTooManyRequests || body["code"] != "too_fast" {
		t.Fatalf("status/code = %d/%v, want 429/too_fast", status, body["code"])
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("нет заголовка Retry-After — машинно разобрать отказ нечем")
	}
}

// TestPublishRESTNotWired — publisher не проброшен (ошибка сборки сервера):
// отвечаем внятным JSON, а не паникой и не текстовым 404 от net/http.
func TestPublishRESTNotWired(t *testing.T) {
	store := openTestStore(t)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /messages", handlePublish(store, nil))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, body := postMessage(t, srv.URL, PublishRequest{Token: "x", Channel: "RU", Text: "y"})
	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501", resp.StatusCode)
	}
	if body["code"] != "not_implemented" {
		t.Errorf("code = %v, want not_implemented", body["code"])
	}
}

// TestPublishRESTBroadcastsOverWS — сообщение, отправленное через REST, приходит
// подписчикам кадром `message` по WS. Это стык двух транспортов и главное, что
// должно работать после переноса отправки: пишем запросом, читаем сокетом.
func TestPublishRESTBroadcastsOverWS(t *testing.T) {
	srv, store := newTestServer(t)
	_, token := publishSession(t, store)

	ws, _, err := websocket.DefaultDialer.Dial(
		"ws"+strings.TrimPrefix(srv.URL, "http")+"/ws?token="+token, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer ws.Close()
	ws.SetReadDeadline(time.Now().Add(5 * time.Second))

	// без locate хаб не знает, на какие каналы подписан этот сокет
	if err := ws.WriteJSON(envelope(TypeLocate, LocateData{Lat: 55.76, Lng: 37.61})); err != nil {
		t.Fatalf("locate: %v", err)
	}
	var located Envelope
	if err := ws.ReadJSON(&located); err != nil {
		t.Fatalf("located: %v", err)
	}

	resp, body := postMessage(t, srv.URL, PublishRequest{
		Token: token, Channel: "RU", Text: "через REST",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("отправка: status %d (%v)", resp.StatusCode, body)
	}

	var env Envelope
	if err := ws.ReadJSON(&env); err != nil {
		t.Fatalf("рассылка не пришла: %v", err)
	}
	if env.Type != TypeMessage {
		t.Fatalf("тип кадра = %q, want message", env.Type)
	}
	var m MessageData
	mustUnmarshal(t, env.Data, &m)
	if m.Text != "через REST" {
		t.Errorf("текст = %q, want «через REST»", m.Text)
	}
	// id в рассылке и в ответе — один и тот же: по нему автор отсеивает эхо
	if got := body["message"].(map[string]any)["id"].(float64); int64(got) != m.ID {
		t.Errorf("id в ответе %v, в рассылке %d — автор не сможет отсеять эхо", got, m.ID)
	}
}

// TestMessagesGetAndPost — /messages это один ресурс: GET читает, POST пишет.
// Проверяем оба метода на одном пути и отказ на остальных.
func TestMessagesGetAndPost(t *testing.T) {
	srv, store := newTestServer(t)
	_, token := publishSession(t, store)

	if resp, body := postMessage(t, srv.URL, PublishRequest{
		Token: token, Channel: "RU", Text: "написали",
	}); resp.StatusCode != http.StatusOK {
		t.Fatalf("POST: status %d (%v)", resp.StatusCode, body)
	}

	resp, err := http.Get(srv.URL + "/messages?channel=RU")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET: status %d", resp.StatusCode)
	}
	var got HistoryData
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Messages) != 1 || got.Messages[0].Text != "написали" {
		t.Errorf("прочитали %v, want одно «написали»", got.Messages)
	}

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/messages", nil)
	del, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	defer del.Body.Close()
	if del.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("DELETE: status %d, want 405 — удаления пока нет", del.StatusCode)
	}
	// 405 отбивает роутер (метод указан в паттерне), и он же перечисляет
	// разрешённые методы — по этому заголовку видно, что ресурс существует, но
	// умеет не то, о чём спросили.
	if allow := del.Header.Get("Allow"); !strings.Contains(allow, "POST") ||
		!strings.Contains(allow, "GET") {
		t.Errorf("Allow = %q, want GET и POST", allow)
	}
}

// TestHistoryAliasStillWorks — прежний путь чтения обязан работать: по нему
// читают сборки из сторов, и снять его можно только вместе с кадром publish.
func TestHistoryAliasStillWorks(t *testing.T) {
	srv, store := newTestServer(t)
	_, token := publishSession(t, store)
	postMessage(t, srv.URL, PublishRequest{Token: token, Channel: "RU", Text: "старым клиентам"})

	resp, err := http.Get(srv.URL + "/history?channel=RU")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	var got HistoryData
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Messages) != 1 {
		t.Errorf("по /history пришло %d сообщений, want 1", len(got.Messages))
	}
}

// TestPublishRetryAfterMute — повтор с тем же client_msg_id отвечает сохранённым
// сообщением, даже если автора успели замьютить. Повтор — не новая публикация, а
// вопрос «дошло ли»; сообщение уже в истории и уже разослано, и отказывать в
// ответе на него значит соврать.
func TestPublishRetryAfterMute(t *testing.T) {
	srv, store := newTestServer(t)
	userID, token := publishSession(t, store)
	req := PublishRequest{
		Token: token, Channel: "RU", Text: "успел", ClientMsgID: "id-1",
	}
	_, first := postMessage(t, srv.URL, req)

	if _, _, err := store.BanTemporary(userID, "спам"); err != nil {
		t.Fatalf("мьют: %v", err)
	}
	resp, second := postMessage(t, srv.URL, req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("повтор после мьюта: status %d (%v)", resp.StatusCode, second)
	}
	if first["message"].(map[string]any)["id"] != second["message"].(map[string]any)["id"] {
		t.Error("повтор вернул другое сообщение")
	}
	// а новая отправка — уже отказ
	if resp, _ := postMessage(t, srv.URL, PublishRequest{
		Token: token, Channel: "RU", Text: "новое", ClientMsgID: "id-2",
	}); resp.StatusCode != http.StatusForbidden {
		t.Errorf("новая отправка под мьютом: status %d, want 403", resp.StatusCode)
	}
}

// TestPublishRetryDoesNotSpendLimit — повтор не тратит токен лимитера. Иначе
// клиент, не получивший ответ по таймауту, за свою же настойчивость получал бы
// «слишком часто» на сообщение, которое давно сохранено.
func TestPublishRetryDoesNotSpendLimit(t *testing.T) {
	_, store := newTestServer(t)
	_, token := publishSession(t, store)
	hub := NewHub()
	go hub.Run()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /messages", handlePublish(store,
		&publisher{store: store, hub: hub, limiter: NewRateLimiter()}))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	req := PublishRequest{Token: token, Channel: "RU", Text: "раз", ClientMsgID: "same"}
	if resp, body := postMessage(t, srv.URL, req); resp.StatusCode != http.StatusOK {
		t.Fatalf("первая отправка: %d (%v)", resp.StatusCode, body)
	}
	// повторяем заведомо больше, чем позволяет всплеск лимитера
	for i := 0; i < messageLimitFor(0, 0).capacity+5; i++ {
		resp, body := postMessage(t, srv.URL, req)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("повтор %d: status %d (%v), want 200", i+1, resp.StatusCode, body)
		}
	}
}

// TestPublishDuplicateReturnsStored — переиспользованный id с ДРУГИМ текстом:
// отвечаем тем, что лежит в базе, а не тем, что пришло в запросе. Иначе клиент
// получил бы 200 про сообщение, которого никто не сохранял и не рассылал.
func TestPublishDuplicateReturnsStored(t *testing.T) {
	srv, store := newTestServer(t)
	_, token := publishSession(t, store)
	postMessage(t, srv.URL, PublishRequest{
		Token: token, Channel: "RU", Text: "настоящее", ClientMsgID: "reused",
	})

	_, body := postMessage(t, srv.URL, PublishRequest{
		Token: token, Channel: "DE", Text: "подменённое", ClientMsgID: "reused",
	})
	m := body["message"].(map[string]any)
	if m["text"] != "настоящее" || m["channel"] != "RU" {
		t.Errorf("ответ = %v, want сохранённое «настоящее» в RU", m)
	}
}

// TestPublishRejectsLongClientMsgID — длинный id отклоняем, а не обрезаем:
// обрезка склеила бы разные id с общим префиксом в один ключ, и сообщение
// потерялось бы под 200 OK.
func TestPublishRejectsLongClientMsgID(t *testing.T) {
	srv, store := newTestServer(t)
	_, token := publishSession(t, store)

	long := strings.Repeat("a", maxClientMsgIDLen+1)
	resp, body := postMessage(t, srv.URL, PublishRequest{
		Token: token, Channel: "RU", Text: "x", ClientMsgID: long,
	})
	if resp.StatusCode != http.StatusBadRequest || body["code"] != "bad_data" {
		t.Errorf("status/code = %d/%v, want 400/bad_data", resp.StatusCode, body["code"])
	}
}
