package main

import "encoding/json"

// Wire-протокол — см. ether-meta/PROTOCOL.md. WS остаётся только там, где
// нужен пуш или живой сокет как побочный эффект (login_telegram — асинхронно
// ждём подтверждения у бота; locate — подписывает соединение; publish/message
// — рассылка). Синхронный запрос-ответ без побочных эффектов на сокете
// (resume, accept_rules, history) вынесен в REST — см. rest.go.
//
// Каждый кадр WebSocket — это Envelope: тег типа + сырой payload, который
// доразбирается по типу.
const (
	// client → server
	TypeLocate        = "locate"         // {lat, lng}
	TypePublish       = "publish"        // {channel, text} — только после authed
	TypeLoginTelegram = "login_telegram" // {} — запросить ссылку входа

	// server → client
	TypeLocated   = "located"    // {channels: [...]}
	TypeMessage   = "message"    // {id, channel, sender, text, ts}
	TypeError     = "error"      // {code, message}
	TypeLoginLink = "login_link" // {url} — deep-link t.me/<бот>?start=<токен>
	TypeAuthed    = "authed"     // {user: {id, nick, username}} — push после подтверждения у бота
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
// ResumeData / AcceptRulesData — тела REST-запросов (см. rest.go), не WS.
type ResumeData struct {
	Token string `json:"token"`
}
type AcceptRulesData struct {
	Token string `json:"token"`
}

// server → client
type LocatedData struct {
	Channels []Channel `json:"channels"`
}
type MessageData struct {
	ID      int64  `json:"id,omitempty"` // курсор для before_id; 0 — не сохранилось
	Channel string `json:"channel"`
	Sender  string `json:"sender"`
	Text    string `json:"text"`
	TS      int64  `json:"ts"`
}

// HistoryData — тело ответа REST GET /history (см. rest.go).
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
// AuthedData — push server → client по WS после подтверждения входа у бота
// (см. telegram.go, TypeAuthed); тот же шейп переиспользует REST-ответ
// POST /session/resume (см. rest.go) — там Token всегда пуст, клиент и так
// прислал его в запросе.
type AuthedData struct {
	User AuthedUser `json:"user"`
	// сессионный токен: клиент сохраняет его и предъявляет в REST /session/resume
	// после реконнекта и в query ?token= при открытии WS; пустой — вне login_telegram
	Token string `json:"token,omitempty"`
	// принимал ли этот аккаунт правила эфира раньше (POST /rules/accept) —
	// привязано к пользователю, не к устройству/сессии; true — клиент минует
	// экран правил
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
