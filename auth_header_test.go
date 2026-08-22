package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
)

// restPostAuth — запрос с токеном в заголовке Authorization и телом БЕЗ поля
// token: так шлёт клиент начиная с 1.4.0.
func restPostAuth(t *testing.T, url, token string, body any) (*http.Response, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post %s: %v", url, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

// TestAuthHeaderOnPublish — отправка с токеном только в заголовке.
func TestAuthHeaderOnPublish(t *testing.T) {
	srv, store := newTestServer(t)
	_, token := publishSession(t, store)

	resp, body := restPostAuth(t, srv.URL+"/messages", token,
		PublishRequest{Channel: "RU", Text: "по заголовку"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%v)", resp.StatusCode, body)
	}
	if got := body["message"].(map[string]any)["text"]; got != "по заголовку" {
		t.Errorf("текст = %v", got)
	}
}

// TestAuthHeaderOnGet — у GET токен тоже переехал в заголовок: раньше он был в
// query, то есть в URL, а оттуда попадал бы в любые access-логи.
func TestAuthHeaderOnGet(t *testing.T) {
	srv, store := newTestServer(t)
	_, token := publishSession(t, store)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/blocked", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

// TestAuthLegacyTokenStillWorks — сборки ≤1.3.0 присылают токен в теле, и это
// обязано работать до их вымирания (см. ether-meta/PLANS.md).
func TestAuthLegacyTokenStillWorks(t *testing.T) {
	srv, store := newTestServer(t)
	_, token := publishSession(t, store)

	resp, body := restPost(t, srv.URL+"/session/resume", ResumeData{Token: token})
	if resp.StatusCode != http.StatusOK {
		t.Errorf("resume с токеном в теле: status %d (%v)", resp.StatusCode, body)
	}
	// и query у GET — тоже прежнее место
	getResp, err := http.Get(srv.URL + "/blocked?token=" + token)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer getResp.Body.Close()
	if getResp.StatusCode != http.StatusOK {
		t.Errorf("blocked с токеном в query: status %d", getResp.StatusCode)
	}
}

// TestAuthHeaderWins — если пришли оба, главенствует заголовок: тело осталось
// только ради старых сборок, и доверять ему больше нельзя.
func TestAuthHeaderWins(t *testing.T) {
	srv, store := newTestServer(t)
	_, token := publishSession(t, store)

	resp, _ := restPostAuth(t, srv.URL+"/session/resume", token,
		ResumeData{Token: "мусор-в-теле"})
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 — заголовок должен перебить тело", resp.StatusCode)
	}
}

// TestAuthBrokenHeader — не-Bearer заголовок считается отсутствующим, и тогда
// работает прежнее место. Иначе чужой Authorization от прокси ломал бы вход.
func TestAuthBrokenHeader(t *testing.T) {
	srv, store := newTestServer(t)
	_, token := publishSession(t, store)

	b, _ := json.Marshal(ResumeData{Token: token})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/session/resume", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 — Basic игнорируем, токен берём из тела", resp.StatusCode)
	}
}

// TestAuthNoTokenAtAll — ни заголовка, ни тела: 400, а не 401. Отсутствие токена
// это неполный запрос, а не мёртвая сессия, и клиент различает их по коду.
func TestAuthNoTokenAtAll(t *testing.T) {
	srv, _ := newTestServer(t)

	resp, body := restPostAuth(t, srv.URL+"/messages", "",
		PublishRequest{Channel: "RU", Text: "x"})
	if resp.StatusCode != http.StatusBadRequest || body["code"] != "bad_data" {
		t.Errorf("status/code = %d/%v, want 400/bad_data", resp.StatusCode, body["code"])
	}
}

// TestAuthBearerCaseInsensitive — схема в Authorization регистронезависима по
// RFC 7235, и HTTP-клиенты этим пользуются.
func TestAuthBearerCaseInsensitive(t *testing.T) {
	srv, store := newTestServer(t)
	_, token := publishSession(t, store)

	b, _ := json.Marshal(ResumeData{})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/session/resume", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

// TestAuthHeaderFiltersBlocked — фильтрация заблокированных авторов в чтении
// сообщений работает и когда токен пришёл заголовком. Это требование Apple 1.2
// («блокировка убирает контент немедленно»), а ветка с заголовком в handleHistory
// единственная, которую легко сломать незаметно: без токена эндпоинт отвечает
// 200 и просто ничего не фильтрует, так что тесты остались бы зелёными.
func TestAuthHeaderFiltersBlocked(t *testing.T) {
	srv, store := newTestServer(t)
	_, reader := publishSession(t, store)
	readerID := mkTgUser(t, store, "777", "alex", "Alex") // тот же, что у сессии
	author := mkTgUser(t, store, "999", "spammer", "Спамер")
	authorToken, err := store.NewSession(author)
	if err != nil {
		t.Fatalf("сессия автора: %v", err)
	}
	restPostAuth(t, srv.URL+"/messages", authorToken,
		PublishRequest{Channel: "RU", Text: "от заблокированного"})

	if err := store.BlockUser(readerID, author); err != nil {
		t.Fatalf("блокировка: %v", err)
	}

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/messages?channel=RU", nil)
	req.Header.Set("Authorization", "Bearer "+reader)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	var got HistoryData
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Messages) != 0 {
		t.Errorf("в выборке %d сообщений заблокированного, want 0", len(got.Messages))
	}
}

// TestDeleteAccountRejectsBrokenBody — испорченное тело с валидным токеном не
// должно удалять аккаунт: удаление необратимо, и «продолжим, тело нам всё равно
// не нужно» здесь неуместно.
func TestDeleteAccountRejectsBrokenBody(t *testing.T) {
	srv, store := newTestServer(t)
	userID, token := publishSession(t, store)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/account/delete",
		bytes.NewReader([]byte(`{"token": "обрез`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	if u, err := store.UserByID(userID); err != nil || u == nil {
		t.Error("аккаунт удалён на битом теле")
	}

	// пустое тело — норма: клиент с 1.4.0 присылает именно его
	resp2, _ := restPostAuth(t, srv.URL+"/account/delete", token, map[string]any{})
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("пустое тело: status %d, want 200", resp2.StatusCode)
	}
}
