package main

import (
	"encoding/json"
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

// Сквозная проверка resume по настоящему WebSocket: publish до входа
// отбивается, мусорный токен — bad_session, посеянный — authed.
// Telegram не участвует: resume ходит только в Store.
func TestResumeOverWebSocket(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "e2e.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	hub := NewHub()
	go hub.Run()
	tg := &TelegramAuth{hub: hub, store: store, pending: map[string]pendingLogin{}}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		c := &Client{hub: hub, conn: conn, send: make(chan Envelope, 16), geo: StubGeocoder{}, tg: tg, store: store}
		hub.register <- c
		go c.writePump()
		go c.readPump()
	}))
	defer srv.Close()

	if err := store.SaveUser(User{TgID: 7, Username: "alex", FirstName: "Alex", Nick: "alex"}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	token, err := store.NewSession(7)
	if err != nil {
		t.Fatalf("seed session: %v", err)
	}

	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer ws.Close()
	ws.SetReadDeadline(time.Now().Add(5 * time.Second))

	roundtrip := func(out Envelope) Envelope {
		if err := ws.WriteJSON(out); err != nil {
			t.Fatalf("write %s: %v", out.Type, err)
		}
		var in Envelope
		if err := ws.ReadJSON(&in); err != nil {
			t.Fatalf("read after %s: %v", out.Type, err)
		}
		return in
	}
	wantError := func(env Envelope, code string) {
		t.Helper()
		if env.Type != TypeError {
			t.Fatalf("got %s %s, want error %s", env.Type, env.Data, code)
		}
		var e ErrorData
		mustUnmarshal(t, env.Data, &e)
		if e.Code != code {
			t.Fatalf("got error %q, want %q", e.Code, code)
		}
	}

	// без входа публиковать нельзя
	wantError(roundtrip(envelope(TypePublish, PublishData{Channel: "RU", Text: "hi"})), "not_authed")

	// мусорный токен не пускает
	wantError(roundtrip(envelope(TypeResume, ResumeData{Token: "garbage"})), "bad_session")

	// настоящий токен восстанавливает вход
	env := roundtrip(envelope(TypeResume, ResumeData{Token: token}))
	var authed AuthedData
	if env.Type != TypeAuthed {
		t.Fatalf("got %s %s, want authed", env.Type, env.Data)
	}
	mustUnmarshal(t, env.Data, &authed)
	if authed.User.ID != 7 || authed.User.Nick != "alex" || authed.Token != token {
		t.Fatalf("authed: %+v", authed)
	}

	// после resume publish проходит: сообщение возвращается подписчику
	roundtrip(envelope(TypeLocate, LocateData{Lat: 55.76, Lng: 37.61})) // located, подписка через StubGeocoder
	msg := roundtrip(envelope(TypePublish, PublishData{Channel: "RU", Text: "привет"}))
	var m MessageData
	if msg.Type != TypeMessage {
		t.Fatalf("got %s %s, want message", msg.Type, msg.Data)
	}
	mustUnmarshal(t, msg.Data, &m)
	if m.Sender != "alex" || m.Text != "привет" || m.ID == 0 {
		t.Fatalf("message: %+v", m)
	}

	// сообщение легло в историю и возвращается кадром history
	env = roundtrip(envelope(TypeHistory, HistoryRequestData{Channel: "RU"}))
	if env.Type != TypeHistory {
		t.Fatalf("got %s %s, want history", env.Type, env.Data)
	}
	var h HistoryData
	mustUnmarshal(t, env.Data, &h)
	if len(h.Messages) != 1 || h.Messages[0].ID != m.ID || h.Messages[0].Text != "привет" {
		t.Fatalf("history: %+v", h)
	}
}
