package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Сквозная проверка лимита через реальный сокет (не только арифметика бакета):
// всплеск проходит, следующее сообщение возвращает error too_fast, а в историю
// отбитое не попадает. Отдельный сервер, потому что newTestServer монтирует /ws
// без лимитера — остальным тестам он мешал бы публиковать подряд.
func TestPublishRateLimitOverWS(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "rl.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	hub := NewHub()
	go hub.Run()
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", wsHandler(hub, StubGeocoder{}, store, nil, NewRateLimiter()))
	mux.HandleFunc("GET /history", handleHistory(store))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	userID := mkTgUser(t, store, "55", "", "flooder")
	token, err := store.NewSession(userID)
	if err != nil {
		t.Fatalf("seed session: %v", err)
	}

	ws, _, err := websocket.DefaultDialer.Dial(
		"ws"+strings.TrimPrefix(srv.URL, "http")+"/ws?token="+token, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer ws.Close()
	ws.SetReadDeadline(time.Now().Add(5 * time.Second))

	// сначала locate: хаб рассылает только подписчикам канала, без него
	// собственное сообщение назад не придёт
	if err := ws.WriteJSON(envelope(TypeLocate, LocateData{Lat: 55.76, Lng: 37.61})); err != nil {
		t.Fatalf("locate: %v", err)
	}
	var located Envelope
	if err := ws.ReadJSON(&located); err != nil {
		t.Fatalf("located: %v", err)
	}

	// аккаунт только что создан — работает узкий тир для свежих (см.
	// newAccountWindow): именно с ним и живёт настоящий новый пользователь
	base := messageLimitFor(0, 0)
	for i := 0; i < base.capacity; i++ {
		if err := ws.WriteJSON(envelope(TypePublish, PublishData{Channel: "RU", Text: "поток"})); err != nil {
			t.Fatalf("publish %d: %v", i+1, err)
		}
		var env Envelope
		if err := ws.ReadJSON(&env); err != nil {
			t.Fatalf("ответ на %d: %v", i+1, err)
		}
		if env.Type != TypeMessage {
			t.Fatalf("сообщение %d: got %q (%s), want message", i+1, env.Type, env.Data)
		}
	}

	// всплеск исчерпан — следующее отбивается
	if err := ws.WriteJSON(envelope(TypePublish, PublishData{Channel: "RU", Text: "лишнее"})); err != nil {
		t.Fatalf("publish over limit: %v", err)
	}
	var env Envelope
	if err := ws.ReadJSON(&env); err != nil {
		t.Fatalf("ответ на превышение: %v", err)
	}
	if env.Type != TypeError {
		t.Fatalf("после всплеска got %q, want error", env.Type)
	}
	var e ErrorData
	mustUnmarshal(t, env.Data, &e)
	if e.Code != "too_fast" {
		t.Fatalf("code = %q (%q), want too_fast", e.Code, e.Message)
	}
	if e.Message == "" {
		t.Fatal("too_fast без человекочитаемого текста — клиенту нечего показать")
	}

	// отбитое сообщение не сохранилось: лимит проверяется ДО записи в историю
	msgs, err := store.History("RU", 0, 100, 0)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(msgs) != base.capacity {
		t.Fatalf("в истории %d сообщений, want %d (отбитое не должно сохраняться)",
			len(msgs), base.capacity)
	}
}

// TestPublishRateLimitSharedAcrossTransports — бакет лимита ОДИН на аккаунт, а не
// на транспорт. Иначе лимит обходился бы чередованием: исчерпал всплеск кадром
// по WS — доотправил через POST /messages.
//
// Раньше это было невозможно случайно (REST-отправки не существовало), а теперь
// достаточно создать лимитер в двух местах — и проверка станет вдвое мягче, чем
// написано в ratelimit.go.
func TestPublishRateLimitSharedAcrossTransports(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "rl-shared.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	hub := NewHub()
	go hub.Run()
	limiter := NewRateLimiter() // один на оба транспорта, как в main.go
	pub := &publisher{store: store, hub: hub, limiter: limiter}

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", wsHandler(hub, StubGeocoder{}, store, nil, limiter))
	mux.HandleFunc("POST /messages", handlePublish(store, pub))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	userID := mkTgUser(t, store, "56", "", "flooder")
	token, err := store.NewSession(userID)
	if err != nil {
		t.Fatalf("seed session: %v", err)
	}

	// весь всплеск съедаем через REST
	base := messageLimitFor(0, 0)
	for i := 0; i < base.capacity; i++ {
		resp, body := restPost(t, srv.URL+"/messages", PublishRequest{
			Token: token, Channel: "RU", Text: "поток",
		})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("REST-отправка %d: status %d (%v)", i+1, resp.StatusCode, body)
		}
	}

	// и следом пробуем добить кадром по WS — тем же аккаунтом
	ws, _, err := websocket.DefaultDialer.Dial(
		"ws"+strings.TrimPrefix(srv.URL, "http")+"/ws?token="+token, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer ws.Close()
	ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	if err := ws.WriteJSON(envelope(TypePublish, PublishData{Channel: "RU", Text: "в обход"})); err != nil {
		t.Fatalf("publish: %v", err)
	}
	var env Envelope
	if err := ws.ReadJSON(&env); err != nil {
		t.Fatalf("ответ: %v", err)
	}
	if env.Type != TypeError {
		t.Fatalf("WS после исчерпанного через REST лимита: got %q, want error", env.Type)
	}
	var e ErrorData
	mustUnmarshal(t, env.Data, &e)
	if e.Code != "too_fast" {
		t.Fatalf("code = %q, want too_fast — бакеты разъехались по транспортам", e.Code)
	}
}
