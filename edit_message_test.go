package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Своё сообщение автор правит, чужое — нет, и правка видна в истории вместе с
// отметкой времени: по ней клиент ставит «изм.».
func TestEditOwnMessageChecksAuthor(t *testing.T) {
	s := openTestStore(t)
	author := mkTgUser(t, s, "1", "author", "Автор")
	stranger := mkTgUser(t, s, "2", "stranger", "Прохожий")
	id := mkMessage(t, s, "RU", author, "превет")

	if _, err := s.EditOwnMessage(stranger, id, "взлом", time.Now()); !errors.Is(err, ErrNotAuthor) {
		t.Fatalf("чужая правка: err = %v, want ErrNotAuthor", err)
	}

	now := time.Now()
	out, err := s.EditOwnMessage(author, id, "привет", now)
	if err != nil {
		t.Fatalf("своя правка: %v", err)
	}
	if !out.Changed || out.Channel != "RU" || out.EditedAt != now.UnixMilli() {
		t.Fatalf("правка = %+v", out)
	}
	msgs, err := s.History("RU", 0, 10, 0)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("история: %v (%d)", err, len(msgs))
	}
	if msgs[0].Text != "привет" || msgs[0].EditedAt != now.UnixMilli() {
		t.Fatalf("в истории text = %q edited_at = %d", msgs[0].Text, msgs[0].EditedAt)
	}
}

// Окно правки короткое: опечатку поправить можно, переписать реплику, под
// которой уже голосовали, — нет. Граница проверяется временем сервера.
func TestEditOwnMessageWindow(t *testing.T) {
	s := openTestStore(t)
	author := mkTgUser(t, s, "1", "author", "Автор")
	sent := time.Now()
	id, _, err := s.SaveMessage("RU", author, "старое", sent.UnixMilli(), "")
	if err != nil {
		t.Fatalf("сохранить: %v", err)
	}

	if _, err := s.EditOwnMessage(author, id, "в окне", sent.Add(editWindow-time.Second)); err != nil {
		t.Fatalf("правка в окне: %v", err)
	}
	_, err = s.EditOwnMessage(author, id, "после окна", sent.Add(editWindow+time.Second))
	if !errors.Is(err, ErrEditExpired) {
		t.Fatalf("правка после окна: err = %v, want ErrEditExpired", err)
	}
	if msgs, _ := s.History("RU", 0, 10, 0); msgs[0].Text != "в окне" {
		t.Fatalf("после отказа текст = %q", msgs[0].Text)
	}
}

// Повтор того же текста — успех без изменений: это ретрай запроса, ответ на
// который потерялся, и он не должен ни двигать время правки, ни рассылать кадр
// второй раз, ни упираться в окно.
func TestEditOwnMessageRepeatIsNoop(t *testing.T) {
	s := openTestStore(t)
	author := mkTgUser(t, s, "1", "author", "Автор")
	id := mkMessage(t, s, "RU", author, "черновик")

	first := time.Now()
	if _, err := s.EditOwnMessage(author, id, "чистовик", first); err != nil {
		t.Fatalf("правка: %v", err)
	}
	out, err := s.EditOwnMessage(author, id, "чистовик", first.Add(editWindow+time.Hour))
	if err != nil {
		t.Fatalf("повтор: err = %v, want nil", err)
	}
	if out.Changed || out.EditedAt != first.UnixMilli() {
		t.Fatalf("повтор = %+v, want Changed=false и прежний edited_at", out)
	}
}

// Сообщения нет — отдельный отказ: клиенту надо убрать его из ленты, а не
// предлагать попробовать ещё раз.
func TestEditOwnMessageGone(t *testing.T) {
	s := openTestStore(t)
	author := mkTgUser(t, s, "1", "author", "Автор")
	if _, err := s.EditOwnMessage(author, 999999, "текст", time.Now()); !errors.Is(err, ErrMessageGone) {
		t.Fatalf("err = %v, want ErrMessageGone", err)
	}
}

// Жалоба переживает правку так же, как удаление: в ней копия текста на момент
// жалобы, иначе правкой стирали бы улику.
func TestEditOwnMessageKeepsReport(t *testing.T) {
	s := openTestStore(t)
	author := mkTgUser(t, s, "1", "author", "Автор")
	reporter := mkTgUser(t, s, "2", "reporter", "Пожаловавшийся")
	id := mkMessage(t, s, "RU", author, "гадость")

	if _, err := s.ReportMessage(id, reporter, "abuse"); err != nil {
		t.Fatalf("жалоба: %v", err)
	}
	if _, err := s.EditOwnMessage(author, id, "ничего такого", time.Now()); err != nil {
		t.Fatalf("правка: %v", err)
	}
	var text string
	if err := s.db.QueryRow(`SELECT message_text FROM reports WHERE message_id = ?`, id).Scan(&text); err != nil {
		t.Fatalf("жалоба: %v", err)
	}
	if text != "гадость" {
		t.Fatalf("копия текста в жалобе = %q — правка её переписала", text)
	}
}

// restPatch — PATCH с телом и токеном в заголовке.
func restPatch(t *testing.T, url, token string, body any) (*http.Response, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPatch, url, bytes.NewReader(b))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("patch %s: %v", url, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

// Коды ответа маршрута: правило проверено выше, здесь — перевод в HTTP.
func TestEditMessageEndpoint(t *testing.T) {
	srv, store := newTestServer(t)
	author, token := publishSession(t, store)
	stranger := mkTgUser(t, store, "888", "other", "Другой")
	strangerToken, err := store.NewSession(stranger)
	if err != nil {
		t.Fatalf("сессия чужого: %v", err)
	}
	url := func(id int64) string { return fmt.Sprintf("%s/messages/%d", srv.URL, id) }
	id := mkMessage(t, store, "RU", author, "превет")

	if resp, body := restPatch(t, url(id), strangerToken, EditData{Text: "x"}); resp.StatusCode != http.StatusForbidden || body["code"] != "forbidden" {
		t.Fatalf("чужое: %d %v, want 403 forbidden", resp.StatusCode, body)
	}

	resp, body := restPatch(t, url(id), token, EditData{Text: "привет"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("своё: %d %v", resp.StatusCode, body)
	}
	if body["text"] != "привет" || body["message_id"] != float64(id) || body["edited_at"] == nil {
		t.Fatalf("ответ = %v", body)
	}

	if resp, _ := restPatch(t, url(id), token, EditData{Text: ""}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("пустой текст: %d, want 400", resp.StatusCode)
	}
	if resp, _ := restPatch(t, url(999999), token, EditData{Text: "x"}); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("нет сообщения: %d, want 404", resp.StatusCode)
	}
	if resp, _ := restPatch(t, srv.URL+"/messages/abc", token, EditData{Text: "x"}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("нечисловой id: %d, want 400", resp.StatusCode)
	}
	if resp, _ := restPatch(t, url(id), "", EditData{Text: "x"}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("без токена: %d, want 400", resp.StatusCode)
	}
	if resp, _ := restPatch(t, url(id), "no-such-token", EditData{Text: "x"}); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("мусорный токен: %d, want 401", resp.StatusCode)
	}

	old, _, err := store.SaveMessage("RU", author, "давнее", time.Now().Add(-editWindow-time.Minute).UnixMilli(), "")
	if err != nil {
		t.Fatalf("сохранить: %v", err)
	}
	if resp, body := restPatch(t, url(old), token, EditData{Text: "x"}); resp.StatusCode != http.StatusForbidden || body["code"] != "edit_expired" {
		t.Fatalf("после окна: %d %v, want 403 edit_expired", resp.StatusCode, body)
	}

	// мьют запрещает правку, как и отправку
	if _, _, err := store.BanTemporary(author, "спам"); err != nil {
		t.Fatalf("бан: %v", err)
	}
	if resp, body := restPatch(t, url(id), token, EditData{Text: "спам"}); resp.StatusCode != http.StatusForbidden || body["code"] != "banned" {
		t.Fatalf("мьют: %d %v, want 403 banned", resp.StatusCode, body)
	}
}

// Кадр `edited` доносит правку до открытых лент: историю они не перезапрашивают.
// Уходит подписчикам канала сообщения и только на настоящей правке — повтор
// того же текста ничего не рассылает.
func TestEditAnnouncesToChannel(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	_, store := newTestServer(t)
	author, token := publishSession(t, store)
	id := mkMessage(t, store, "RU", author, "превет")

	mux := http.NewServeMux()
	mux.HandleFunc("PATCH /messages/{id}", handleEditMessage(store, hub))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	reader := &Client{send: make(chan Envelope, 8)}
	bystander := &Client{send: make(chan Envelope, 8)}
	hub.subscribe <- subscription{client: reader, channels: []string{"RU"}}
	hub.subscribe <- subscription{client: bystander, channels: []string{"DE"}}

	url := fmt.Sprintf("%s/messages/%d", srv.URL, id)
	if resp, body := restPatch(t, url, token, EditData{Text: "привет"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("правка: %d %v", resp.StatusCode, body)
	}
	select {
	case env := <-reader.send:
		if env.Type != TypeEdited {
			t.Fatalf("тип кадра %q, want %q", env.Type, TypeEdited)
		}
		var d EditedData
		mustUnmarshal(t, env.Data, &d)
		if d.MessageID != id || d.Text != "привет" || d.EditedAt == 0 {
			t.Fatalf("edited = %+v", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("кадр edited не пришёл — правка не доедет до открытых лент")
	}

	// повтор: кадра нет
	if resp, _ := restPatch(t, url, token, EditData{Text: "привет"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("повтор: %d", resp.StatusCode)
	}
	select {
	case env := <-reader.send:
		t.Fatalf("повтор разослал кадр: %+v", env)
	case <-time.After(100 * time.Millisecond):
	}
	select {
	case env := <-bystander.send:
		t.Fatalf("кадр уехал в чужой канал: %+v", env)
	default:
	}
}
