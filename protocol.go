package main

import "encoding/json"

// Wire-протокол — см. ether-meta/PROTOCOL.md. Каждый кадр WebSocket — это
// Envelope: тег типа + сырой payload, который доразбирается по типу.
const (
	// client → server
	TypeLocate        = "locate"         // {lat, lng}
	TypePublish       = "publish"        // {channel, text} — только после authed
	TypeLoginTelegram = "login_telegram" // {} — запросить ссылку входа
	TypeResume        = "resume"         // {token} — восстановить сессию после реконнекта
	TypeAcceptRules   = "accept_rules"   // {} — принять правила эфира, только после authed

	// history: запрос client → server {channel, before_id?, limit?},
	// ответ server → client {channel, messages: [...]}
	TypeHistory = "history"

	// server → client
	TypeLocated   = "located"    // {channels: [...]}
	TypeMessage   = "message"    // {id, channel, sender, text, ts}
	TypeError     = "error"      // {code, message}
	TypeLoginLink = "login_link" // {url} — deep-link t.me/<бот>?start=<токен>
	TypeAuthed    = "authed"     // {user: {id, nick, username}}
)

// Envelope — внешняя оболочка любого сообщения.
type Envelope struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data,omitempty"`
}

// client → server
type LocateData struct {
	Lat float64 `json:"lat"`
	Lng float64 `json:"lng"`
}
type PublishData struct {
	Channel string `json:"channel"`
	Text    string `json:"text"`
}
type ResumeData struct {
	Token string `json:"token"`
}
type HistoryRequestData struct {
	Channel  string `json:"channel"`
	BeforeID int64  `json:"before_id,omitempty"` // 0 — с конца; иначе сообщения старше этого id
	Limit    int    `json:"limit,omitempty"`     // 0 → 50, максимум 200
}

// server → client
type LocatedData struct {
	Channels []Channel `json:"channels"`
}
type MessageData struct {
	ID      int64  `json:"id,omitempty"` // курсор для history/before_id; 0 — не сохранилось
	Channel string `json:"channel"`
	Sender  string `json:"sender"`
	Text    string `json:"text"`
	TS      int64  `json:"ts"`
}
type HistoryData struct {
	Channel  string        `json:"channel"`
	Messages []MessageData `json:"messages"` // хронологически, по возрастанию id
}
type ErrorData struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
type LoginLinkData struct {
	URL string `json:"url"`
}
type AuthedUser struct {
	ID       int64  `json:"id"` // Telegram user id
	Nick     string `json:"nick"`
	Username string `json:"username,omitempty"`
}
type AuthedData struct {
	User AuthedUser `json:"user"`
	// сессионный токен: клиент сохраняет его и предъявляет в resume после
	// реконнекта; пустой, если сессию не удалось сохранить
	Token string `json:"token,omitempty"`
	// принимал ли этот аккаунт правила эфира раньше (accept_rules) — привязано
	// к пользователю, не к устройству/сессии; true — клиент минует экран правил
	RulesAccepted bool `json:"rules_accepted"`
}

// mustJSON сериализует payload в RawMessage для вложения в Envelope.
func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func envelope(typ string, data any) Envelope {
	return Envelope{Type: typ, Data: mustJSON(data)}
}
