package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// fakeTransport перехватывает запросы к api.telegram.org: адрес там зашит в
// Notifier.send, зато http.Client подменяемый — этого хватает, чтобы проверить
// и факт отправки, и содержимое.
type fakeTransport struct {
	sent chan map[string]any
}

func (f *fakeTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	var payload map[string]any
	if r.Body != nil {
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &payload)
	}
	payload["__url"] = r.URL.String()
	f.sent <- payload
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
		Header:     make(http.Header),
	}, nil
}

// newFakeNotifier — Notifier с перехваченным транспортом (конструктор напрямую:
// NewNotifier создал бы обычный http.Client).
func newFakeNotifier() (*Notifier, chan map[string]any) {
	sent := make(chan map[string]any, 4)
	return &Notifier{
		token:  "bot-token",
		chatID: "-100500",
		http:   &http.Client{Transport: &fakeTransport{sent: sent}},
	}, sent
}

func TestNotifierAccountCreated(t *testing.T) {
	n, sent := newFakeNotifier()

	n.AccountCreated(User{ID: 777, TgUsername: "alex_tg", FullName: "Alex <b>"}, ProviderTelegram)

	got := <-sent
	if !strings.Contains(got["__url"].(string), "/botbot-token/sendMessage") {
		t.Errorf("url = %v", got["__url"])
	}
	if got["chat_id"] != "-100500" || got["parse_mode"] != "HTML" {
		t.Errorf("payload = %+v", got)
	}
	text, _ := got["text"].(string)
	for _, want := range []string{"Новый пользователь", "@alex_tg", "777", "telegram"} {
		if !strings.Contains(text, want) {
			t.Errorf("text %q не содержит %q", text, want)
		}
	}
	// пользовательское имя экранируется, иначе "<b>" сломает разбор HTML у Telegram
	if strings.Contains(text, "Alex <b>") {
		t.Errorf("имя не экранировано: %q", text)
	}
	if !strings.Contains(text, "&lt;b&gt;") {
		t.Errorf("ожидалось экранированное имя, got %q", text)
	}
}

// без имени и @username уведомление всё равно осмысленно
func TestNotifierAccountCreatedNoName(t *testing.T) {
	n, sent := newFakeNotifier()
	n.AccountCreated(User{ID: 42}, ProviderApple)
	text := (<-sent)["text"].(string)
	if !strings.Contains(text, "без имени") || !strings.Contains(text, "42") {
		t.Errorf("text = %q", text)
	}
}
