package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
)

// Свои сообщения автор удаляет, чужие — нет. Проверка владельца здесь несущая:
// без неё DELETE превращается в модерацию, доступную кому угодно.
func TestDeleteOwnMessageChecksAuthor(t *testing.T) {
	s := openTestStore(t)
	author := mkTgUser(t, s, "1", "author", "Автор")
	stranger := mkTgUser(t, s, "2", "stranger", "Прохожий")
	id := mkMessage(t, s, "RU", author, "моё сообщение")

	// чужому нельзя, и сообщение остаётся на месте
	switch _, err := s.DeleteOwnMessage(stranger, id); {
	case !errors.Is(err, ErrNotAuthor):
		t.Fatalf("чужое удаление: err = %v, want ErrNotAuthor", err)
	}
	msgs, err := s.History("RU", 0, 10, author)
	if err != nil {
		t.Fatalf("история: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("после отказа сообщений = %d, want 1", len(msgs))
	}

	// своё удаляется
	removed, err := s.DeleteOwnMessage(author, id)
	if err != nil || !removed {
		t.Fatalf("своё удаление: removed = %v, err = %v", removed, err)
	}
	msgs, err = s.History("RU", 0, 10, author)
	if err != nil {
		t.Fatalf("история: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("после удаления сообщений = %d, want 0", len(msgs))
	}
}

// Повтор — успех, а не «не найдено». DELETE идемпотентен по определению метода,
// и на этом держится безопасность ретрая после обрыва сети: клиент повторяет
// запрос, не зная, дошёл ли первый.
func TestDeleteOwnMessageIsIdempotent(t *testing.T) {
	s := openTestStore(t)
	author := mkTgUser(t, s, "1", "author", "Автор")
	id := mkMessage(t, s, "RU", author, "моё сообщение")

	if removed, err := s.DeleteOwnMessage(author, id); err != nil || !removed {
		t.Fatalf("первое удаление: removed = %v, err = %v", removed, err)
	}
	// второй раз: removed=false, но НЕ ошибка — обработчик ответит 200
	removed, err := s.DeleteOwnMessage(author, id)
	if err != nil {
		t.Fatalf("повтор удаления: err = %v, want nil", err)
	}
	if removed {
		t.Fatal("повтор удаления сказал removed=true, хотя удалять было нечего")
	}
	// то же для id, которого не было никогда
	if removed, err := s.DeleteOwnMessage(author, 999999); err != nil || removed {
		t.Fatalf("несуществующий id: removed = %v, err = %v", removed, err)
	}
}

// Удаление возвращает проголосовавшим их голоса — тем же каскадом, которым это
// делает уборка по TTL. Отдельного кода для этого нет и не должно быть.
func TestDeleteOwnMessageReturnsVotes(t *testing.T) {
	s := openTestStore(t)
	author := mkTgUser(t, s, "1", "author", "Автор")
	fan := mkTgUser(t, s, "2", "fan", "Читатель")
	id := mkMessage(t, s, "RU", author, "оценят и удалю")

	before, err := s.VotesLeft(fan)
	if err != nil {
		t.Fatalf("запас до голоса: %v", err)
	}
	if _, err := s.Vote(fan, id, 1); err != nil {
		t.Fatalf("голос: %v", err)
	}
	if left, _ := s.VotesLeft(fan); left != before-1 {
		t.Fatalf("запас после голоса = %d, want %d", left, before-1)
	}

	if _, err := s.DeleteOwnMessage(author, id); err != nil {
		t.Fatalf("удаление: %v", err)
	}
	if left, _ := s.VotesLeft(fan); left != before {
		t.Fatalf("запас после удаления = %d, want %d — голос не вернулся", left, before)
	}
	// рейтинг автора тоже теряет этот голос: удалять минусы, чтобы разогнаться,
	// бессмысленно — но и плюсы удаление уносит
	if r, _ := s.AuthorRating(author); r != 0 {
		t.Fatalf("рейтинг автора после удаления = %d, want 0", r)
	}
}

// Жалоба переживает удаление. Иначе автор стирал бы улику против себя: пожаловались
// — удалил — модератору нечего разбирать.
func TestDeleteOwnMessageKeepsReport(t *testing.T) {
	s := openTestStore(t)
	author := mkTgUser(t, s, "1", "author", "Автор")
	reporter := mkTgUser(t, s, "2", "reporter", "Пожаловавшийся")
	id := mkMessage(t, s, "RU", author, "на это пожалуются")

	if _, err := s.ReportMessage(id, reporter, "spam"); err != nil {
		t.Fatalf("жалоба: %v", err)
	}
	if _, err := s.DeleteOwnMessage(author, id); err != nil {
		t.Fatalf("удаление: %v", err)
	}

	var n int
	var text string
	var reportedAuthor int64
	err := s.db.QueryRow(
		`SELECT COUNT(*), MAX(message_text), MAX(author_user_id) FROM reports WHERE message_id = ?`,
		id,
	).Scan(&n, &text, &reportedAuthor)
	if err != nil {
		t.Fatalf("чтение жалоб: %v", err)
	}
	if n != 1 {
		t.Fatalf("жалоб после удаления = %d, want 1", n)
	}
	if text != "на это пожалуются" {
		t.Fatalf("копия текста в жалобе = %q — удаление её унесло", text)
	}
	if reportedAuthor != author {
		t.Fatalf("автор в жалобе = %d, want %d", reportedAuthor, author)
	}
}

// restDelete — DELETE без тела: id в пути, токен в заголовке.
func restDelete(t *testing.T, url, token string) (*http.Response, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, url, nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("delete %s: %v", url, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

// Коды ответа маршрута. Отдельно от store-тестов: там проверяется правило, здесь
// — что обработчик переводит его в правильный HTTP.
func TestDeleteMessageEndpoint(t *testing.T) {
	srv, store := newTestServer(t)
	author, token := publishSession(t, store)
	stranger := mkTgUser(t, store, "888", "other", "Другой")
	strangerToken, err := store.NewSession(stranger)
	if err != nil {
		t.Fatalf("сессия чужого: %v", err)
	}
	url := func(id int64) string { return fmt.Sprintf("%s/messages/%d", srv.URL, id) }

	id := mkMessage(t, store, "RU", author, "удалю сам")

	// чужой — 403, сообщение на месте
	if resp, body := restDelete(t, url(id), strangerToken); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("чужое: status = %d, want 403 (%v)", resp.StatusCode, body)
	}
	if msgs, _ := store.History("RU", 0, 10, author); len(msgs) != 1 {
		t.Fatalf("после 403 сообщений = %d, want 1", len(msgs))
	}

	// автор — 200, и сообщения больше нет
	if resp, body := restDelete(t, url(id), token); resp.StatusCode != http.StatusOK {
		t.Fatalf("своё: status = %d, want 200 (%v)", resp.StatusCode, body)
	}
	if msgs, _ := store.History("RU", 0, 10, author); len(msgs) != 0 {
		t.Fatalf("после удаления сообщений = %d, want 0", len(msgs))
	}

	// повтор — тоже 200: DELETE идемпотентен, ретрай не должен выглядеть ошибкой
	if resp, body := restDelete(t, url(id), token); resp.StatusCode != http.StatusOK {
		t.Fatalf("повтор: status = %d, want 200 (%v)", resp.StatusCode, body)
	}

	// нечисловой id — 400, а не 404 от роутера
	if resp, _ := restDelete(t, srv.URL+"/messages/abc", token); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("нечисловой id: status = %d, want 400", resp.StatusCode)
	}

	// без токена — 400 (нечего проверять), с мусорным — 401
	if resp, _ := restDelete(t, url(id), ""); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("без токена: status = %d, want 400", resp.StatusCode)
	}
	if resp, _ := restDelete(t, url(id), "no-such-token"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("мусорный токен: status = %d, want 401", resp.StatusCode)
	}

	// GET /messages как был, так и остался: паттерн с {id} не перехватывает коллекцию
	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/messages", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("delete коллекции: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE /messages: status = %d, want 405", resp.StatusCode)
	}
}
