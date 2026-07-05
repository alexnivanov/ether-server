package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// TelegramAuth — вход через Telegram по deep-link боту (см. PROTOCOL.md):
// клиент запрашивает login_link, получает t.me/<бот>?start=<токен>, пользователь
// жмёт Start, бот видит `/start <токен>`, и сервер аутентифицирует то соединение,
// которое запросило токен. Виджет Login Widget не используем: ему нужен домен
// (/setdomain), deep-link работает и с localhost, и в мобильных клиентах.
//
// Токен бота приходит из конфига (config.json, см. config.example.json).
// Вход через Telegram обязателен: без валидного токена сервер не стартует.
type TelegramAuth struct {
	token   string
	botName string
	hub     *Hub
	store   *Store

	mu      sync.Mutex
	pending map[string]pendingLogin // login-токен → кто его ждёт
}

type pendingLogin struct {
	client  *Client
	expires time.Time
}

const loginTTL = 5 * time.Minute

func NewTelegramAuth(botToken string, hub *Hub, store *Store) (*TelegramAuth, error) {
	t := &TelegramAuth{token: botToken, hub: hub, store: store, pending: map[string]pendingLogin{}}
	name, err := t.getMe()
	if err != nil {
		return nil, err // api() уже включает имя метода в текст ошибки
	}
	t.botName = name
	go t.poll()
	go t.expireLoop()
	log.Printf("telegram: вход включён через @%s", name)
	return t, nil
}

// NewLoginToken регистрирует одноразовый токен для соединения и возвращает
// deep-link, который клиент откроет у пользователя.
func (t *TelegramAuth) NewLoginToken(c *Client) string {
	b := make([]byte, 16)
	rand.Read(b)
	token := hex.EncodeToString(b)
	t.mu.Lock()
	t.pending[token] = pendingLogin{client: c, expires: time.Now().Add(loginTTL)}
	t.mu.Unlock()
	return "https://t.me/" + t.botName + "?start=" + token
}

// Cancel снимает все ожидающие токены соединения. Вызывается до unregister в
// хабе — после этого confirm уже не найдёт клиента и не тронет закрытый send.
func (t *TelegramAuth) Cancel(c *Client) {
	t.mu.Lock()
	for token, p := range t.pending {
		if p.client == c {
			delete(t.pending, token)
		}
	}
	t.mu.Unlock()
}

// ── обработка апдейтов бота ──

type tgUpdate struct {
	UpdateID int64 `json:"update_id"`
	Message  *struct {
		Text string `json:"text"`
		From *struct {
			ID        int64  `json:"id"`
			FirstName string `json:"first_name"`
			Username  string `json:"username"`
		} `json:"from"`
		Chat struct {
			ID int64 `json:"id"`
		} `json:"chat"`
	} `json:"message"`
}

func (t *TelegramAuth) poll() {
	offset := int64(0)
	for {
		var updates []tgUpdate
		err := t.api("getUpdates", url.Values{
			"timeout":         {"50"},
			"offset":          {fmt.Sprint(offset)},
			"allowed_updates": {`["message"]`},
		}, &updates)
		if err != nil {
			log.Printf("telegram: getUpdates: %v", err)
			time.Sleep(3 * time.Second)
			continue
		}
		for _, u := range updates {
			offset = u.UpdateID + 1
			m := u.Message
			if m == nil || m.From == nil {
				continue
			}
			token, ok := strings.CutPrefix(m.Text, "/start ")
			if !ok {
				t.reply(m.Chat.ID, "Это бот входа в Эфир — нажми «Продолжить с Telegram» в приложении.")
				continue
			}
			t.confirm(strings.TrimSpace(token), m.From.ID, m.From.Username, m.From.FirstName, m.Chat.ID)
		}
	}
}

func (t *TelegramAuth) confirm(token string, id int64, username, firstName string, chatID int64) {
	t.mu.Lock()
	p, ok := t.pending[token]
	if ok {
		delete(t.pending, token)
	}
	t.mu.Unlock()
	if !ok || time.Now().After(p.expires) {
		t.reply(chatID, "Ссылка устарела — запроси вход в приложении ещё раз.")
		return
	}

	nick := username
	if nick == "" {
		nick = firstName
	}
	if nick == "" {
		nick = "anon"
	}

	// персистентность: пользователь и токен сессии переживают реконнект
	// (кадр resume). Ошибка хранилища не валит вход — просто без resume.
	sessionToken := ""
	rulesAccepted := false
	if accepted, err := t.store.SaveUser(User{TgID: id, Username: username, FirstName: firstName, Nick: nick}); err != nil {
		log.Printf("telegram: save user %d: %v", id, err)
	} else {
		rulesAccepted = accepted
		if sessionToken, err = t.store.NewSession(id); err != nil {
			log.Printf("telegram: new session for %d: %v", id, err)
			sessionToken = ""
		}
	}

	p.client.setAuthed(id, nick)
	// доставка через хаб: он сериализует отправку с close(send) при unregister
	t.hub.direct <- directEnvelope{
		client: p.client,
		env: envelope(TypeAuthed, AuthedData{
			User:          AuthedUser{ID: id, Nick: nick, Username: username},
			Token:         sessionToken,
			RulesAccepted: rulesAccepted,
		}),
	}
	t.reply(chatID, "Готово, "+nick+"! Возвращайся в Эфир 🎉")
	log.Printf("telegram: authed %q (tg id %d)", nick, id)
}

func (t *TelegramAuth) expireLoop() {
	for range time.Tick(time.Minute) {
		now := time.Now()
		t.mu.Lock()
		for token, p := range t.pending {
			if now.After(p.expires) {
				delete(t.pending, token)
			}
		}
		t.mu.Unlock()
	}
}

// ── Bot API ──

// клиент с запасом по таймауту под long polling (timeout=50)
var tgHTTP = &http.Client{Timeout: 60 * time.Second}

func (t *TelegramAuth) api(method string, params url.Values, out any) error {
	resp, err := tgHTTP.PostForm("https://api.telegram.org/bot"+t.token+"/"+method, params)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	var env struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		Description string          `json:"description"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	if !env.OK {
		return fmt.Errorf("%s: %s", method, env.Description)
	}
	if out != nil {
		return json.Unmarshal(env.Result, out)
	}
	return nil
}

func (t *TelegramAuth) getMe() (string, error) {
	var me struct {
		Username string `json:"username"`
	}
	if err := t.api("getMe", url.Values{}, &me); err != nil {
		return "", err
	}
	return me.Username, nil
}

func (t *TelegramAuth) reply(chatID int64, text string) {
	err := t.api("sendMessage", url.Values{
		"chat_id": {fmt.Sprint(chatID)},
		"text":    {text},
	}, nil)
	if err != nil {
		log.Printf("telegram: sendMessage: %v", err)
	}
}
