package main

import "encoding/json"

// Wire-протокол — см. ether-meta/PROTOCOL.md. WS остаётся только там, где нужен
// пуш или живой сокет как побочный эффект: locate — подписывает соединение;
// publish/message — рассылка. Сокет авторизуется единственным способом —
// токеном сессии в query ?token= при апгрейде (см. wsHandler). Аутентификация
// (вход через нативный Telegram Login SDK), resume, accept_rules, history — в
// REST (см. rest.go).
//
// Каждый кадр WebSocket — это Envelope: тег типа + сырой payload, который
// доразбирается по типу.
const (
	// client → server
	TypeLocate  = "locate"  // {lat, lng}
	TypePublish = "publish" // {channel, text} — только на authed-сокете

	// server → client
	TypeLocated = "located" // {channels: [...]}
	TypeMessage = "message" // {id, channel, sender_id, sender, username, avatar_url, text, ts}
	TypeError   = "error"   // {code, message}
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

// ResumeData / AcceptRulesData / LogoutData — тела REST-запросов (см. rest.go),
// не WS. Разнесены по типу на запрос ради читаемости, хотя поле одно и то же.
type ResumeData struct {
	Token string `json:"token"`
}
type AcceptRulesData struct {
	Token string `json:"token"`
}
type LogoutData struct {
	Token string `json:"token"`
}

// TelegramAuthRequest — тело POST /auth/telegram: OIDC ID-token от нативного
// Telegram Login SDK, сервер проверяет его подпись по JWKS (см. telegram.go).
type TelegramAuthRequest struct {
	IDToken string `json:"id_token"`
}

// server → client
type LocatedData struct {
	Channels []Channel `json:"channels"`
}

// MessageData — сообщение для клиента. Sender/Username/AvatarURL не хранятся в
// таблице messages (там только tg_id автора): для истории собираются JOIN из
// users, для live — из авторского соединения. SenderID/Username нужны клиенту,
// чтобы по тапу на аватар открыть профиль автора в Telegram. AvatarURL/Username
// пустые — у автора нет фото / нет @username.
type MessageData struct {
	ID        int64  `json:"id,omitempty"` // курсор для before_id; 0 — не сохранилось
	Channel   string `json:"channel"`
	SenderID  int64  `json:"sender_id,omitempty"` // Telegram user id автора
	Sender    string `json:"sender"`
	Username  string `json:"username,omitempty"`
	AvatarURL string `json:"avatar_url,omitempty"`
	Text      string `json:"text"`
	TS        int64  `json:"ts"`
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
type AuthedUser struct {
	ID        int64  `json:"id"`                   // Telegram user id
	Username  string `json:"username,omitempty"`   // @username — ссылка на профиль
	Name      string `json:"name,omitempty"`       // отображаемое имя (единственное для UI)
	AvatarURL string `json:"avatar_url,omitempty"` // URL фото профиля из Telegram
}

// AuthedData — общий шейп REST-ответов про личность: POST /auth/telegram (вход
// через Telegram Login SDK), POST /session/resume и POST /rules/accept (см. rest.go).
// В resume/accept_rules поле Token пустое — клиент и так прислал его в запросе;
// заполнено оно только в ответе /auth/telegram (новая сессия).
type AuthedData struct {
	User AuthedUser `json:"user"`
	// сессионный токен: клиент сохраняет его и предъявляет в REST /session/resume
	// после реконнекта и в query ?token= при открытии WS; пустой — вне /auth/telegram
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
