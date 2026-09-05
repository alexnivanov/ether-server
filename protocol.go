package main

import "encoding/json"

// Wire-протокол — см. ether-meta/PROTOCOL.md. WS остаётся только там, где нужен
// пуш или живой сокет как побочный эффект: locate — подписывает соединение,
// message — рассылка. Сокет авторизуется единственным способом — токеном сессии
// в query ?token= при апгрейде (см. wsHandler). Всё остальное — в REST
// (см. rest.go): вход через провайдера (Telegram/Apple/Google), resume,
// accept_rules, чтение и отправка сообщений.
//
// Каждый кадр WebSocket — это Envelope: тег типа + сырой payload, который
// доразбирается по типу.
const (
	// client → server
	TypeLocate = "locate" // {lat, lng}

	// server → client
	TypeLocated = "located" // {channels: [...]}
	TypeMessage = "message" // {id, channel, sender_id, sender, username, avatar_url, text, ts}
	TypeRemoved = "removed" // {message_id} | {user_id} — модератор убрал контент
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

// SetNameData — тело POST /profile/name: отображаемое имя, заданное вручную.
// Отдельный запрос, а не поле входа: имя может понадобиться задать и позже
// (аккаунт остался без имени, потому что провайдер его не дал).
type SetNameData struct {
	Name string `json:"name"`
}

// BlockData — тело POST /block: кого блокируем (внутренний id) и, опционально,
// текст сообщения, из-за которого это делают, — он уходит в уведомление
// модератору (Apple 1.2 требует, чтобы блокировка сообщала разработчику о
// контенте). Unblock=true — обратная операция тем же эндпоинтом: отдельный путь
// ради одного флага не нужен.
type BlockData struct {
	UserID      int64  `json:"user_id"`
	MessageText string `json:"message_text,omitempty"`
	Unblock     bool   `json:"unblock,omitempty"`
}

// BlockedData — тело ответа GET /blocked: заблокированные с профилями.
type BlockedData struct {
	Users []BlockedUser `json:"users"`
}

// LinkRequest — тело POST /profile/link/{провайдер}: ID-token провайдера (что
// привязываем); к какому аккаунту — известно из токена сессии в заголовке.
// Name — как в AuthRequest, нужен только Apple.
type LinkRequest struct {
	IDToken string `json:"id_token"`
	Name    string `json:"name,omitempty"`
}

// PushTokenData — тело POST /push/register и POST /push/unregister: FCM-токен
// устройства. Сервер шлёт пуши адресно по токенам (а не в топики), чтобы не
// уведомлять автора о его же сообщении, — см. push.go. Platform (ios|android)
// нужен только для диагностики.
type PushTokenData struct {
	FCMToken string `json:"fcm_token"` // токен устройства от FCM
	Platform string `json:"platform,omitempty"`
}

// ReportData — тело POST /report: жалоба на сообщение (модерация UGC). Текст и
// автора сервер берёт из самого сообщения по MessageID — клиент присылает лишь
// на что жалуется и почему. Reason — код причины из фиксированного набора
// клиента (spam/abuse/illegal/other), свободного текста нет.
type ReportData struct {
	MessageID int64  `json:"message_id"`
	Reason    string `json:"reason,omitempty"`
}

// AuthRequest — тело POST /auth/{telegram,apple,google}: ID-token от провайдера,
// сервер проверяет его подпись по публичным ключам провайдера (JWKS, см. oidc.go).
//
// Name нужен ровно для Apple: Sign in with Apple отдаёт имя ОДИН раз, при первой
// авторизации, и не внутри токена, а в ответе системного диалога — значит
// прислать его может только клиент. У остальных провайдеров имя есть в токене, и
// это поле игнорируется.
type AuthRequest struct {
	IDToken string `json:"id_token"`
	Name    string `json:"name,omitempty"`
}

// server → client
type LocatedData struct {
	Channels []Channel `json:"channels"`
}

// MessageData — сообщение для клиента. Sender/Username/AvatarURL не хранятся в
// таблице messages (там только внутренний id автора): для истории собираются
// JOIN из users, для live — из авторского соединения. SenderID нужен клиенту,
// чтобы отличать свои сообщения; Username — чтобы открыть профиль автора в
// Telegram. AvatarURL/Username пустые — у автора нет фото / нет @username
// (у входа через Apple их не бывает вовсе).
type MessageData struct {
	ID        int64  `json:"id,omitempty"` // курсор для before_id; 0 — не сохранилось
	Channel   string `json:"channel"`
	SenderID  int64  `json:"sender_id,omitempty"` // внутренний id автора (не id у провайдера)
	Sender    string `json:"sender"`
	Username  string `json:"username,omitempty"`
	AvatarURL string `json:"avatar_url,omitempty"`
	Text      string `json:"text"`
	TS        int64  `json:"ts"`
	// Rating — сумма голосов под сообщением (плюс и минус стоят одинаково).
	// omitempty намеренно: нуля клиент не показывает, а «нет голосов» и «плюс с
	// минусом погасились» для читателя одно и то же — информации в нуле нет.
	//
	// Сумма видна, но реплику не прячет: авто-скрытие минусовых сообщений
	// разобрано и отвергнуто. Лента живая — сообщение ушло по WS мгновенно, минусы
	// приходят потом, и скрытие подействовало бы лишь на тех, кто откроет историю
	// позже; кворум не выбрать, не зная числа активных в канале; да и голосуют
	// вообще за то, что можно перечитать, а реплика проскроллила за десять секунд.
	// Единственное, на что влияет рейтинг, — темп автора (ratelimit.go).
	Rating int `json:"rating,omitempty"`
	// MyVote — как проголосовал тот, кто запросил историю: +1 | -1 | 0. Нужен
	// отдельно от Rating: свой плюс мог быть погашен чужим минусом, но кнопка у
	// человека всё равно должна выглядеть нажатой. В live-рассылке всегда 0 —
	// новое сообщение ещё никто не отметил.
	MyVote int `json:"my_vote,omitempty"`
}

// VoteData — тело POST /vote: за какое сообщение и как. Value: +1 | -1 | 0, где
// 0 — снять свой голос (обратная операция тем же эндпоинтом, как Unblock в
// BlockData). Автора сервер берёт из самого сообщения: клиент присылает только
// message_id.
type VoteData struct {
	MessageID int64 `json:"message_id"`
	Value     int   `json:"value"`
}

// VoteResultData — тело ответа POST /vote: новое состояние сообщения и остаток
// запаса. Остаток здесь, а не отдельным запросом: он меняется этим же действием,
// и клиенту нужно обновить подпись у кнопки сразу.
type VoteResultData struct {
	MessageID int64 `json:"message_id"`
	Rating    int   `json:"rating"`
	MyVote    int   `json:"my_vote"`
	VotesLeft int   `json:"votes_left"`
}

// RemovedData — контент убран: либо одно сообщение (MessageID), либо все
// сообщения автора (UserID — при `/purge` и при постоянном бане, где аккаунт
// удаляется вместе с сообщениями).
//
// Источников два: модератор и сам автор (DELETE /messages/{id}). Кадр у них
// общий — клиенту незачем знать, кто удалил, действие для него одно и то же.
//
// Зачем кадр вообще: удаление убирает строку в БД, но у подключённых клиентов
// сообщение остаётся в ленте до перезапуска — они её не перезапрашивают.
// Получив `removed`, клиент убирает сообщение сразу.
type RemovedData struct {
	MessageID int64 `json:"message_id,omitempty"`
	UserID    int64 `json:"user_id,omitempty"`
}

// HealthData — тело ответа GET /health: то, что нужно внешнему пингеру и
// deploy.sh. Намеренно без статистики (число пользователей, сообщений):
// эндпоинт публичный, а масштаб проекта — не то, что стоит отдавать всем.
type HealthData struct {
	OK        bool   `json:"ok"`
	Version   string `json:"version"`
	DB        string `json:"db"`         // ok | текст ошибки
	UptimeSec int64  `json:"uptime_sec"` // сколько живёт процесс
}

// UpdateData — тело ответа GET /version: вердикт о версии клиента (см.
// version.go). Latest — что считать свежей версией, для показа человеку; URL —
// куда ведёт кнопка «Обновить», заполняется только когда обновляться есть зачем.
//
// Имя не VersionData: version в этом сервере — его собственная версия сборки
// (см. HealthData), и две «версии» с разным смыслом рядом путали бы.
type UpdateData struct {
	Status string `json:"status"` // ok | soft | required
	Latest string `json:"latest,omitempty"`
	URL    string `json:"url,omitempty"`
}

// PublishRequest — тело POST /messages; токен сессии — заголовком.
//
// ClientMsgID — id отправки, придуманный клиентом; повтор с тем же id не
// публикует второе сообщение, а возвращает уже сохранённое. Ради этого отправка
// когда-то и уехала с WS: кадр уходил в буфер сокета без подтверждения, и
// повторить его было нельзя. Необязателен: без него запрос работает, просто
// повтор создаст дубль.
type PublishRequest struct {
	Channel     string `json:"channel"`
	Text        string `json:"text"`
	ClientMsgID string `json:"client_msg_id,omitempty"`
}

// PublishedData — тело ответа POST /messages: опубликованное сообщение тем же
// шейпом, что кадр `message` на WS. Автору оно придёт ещё и рассылкой — клиент
// отсеет дубль по серверному id.
type PublishedData struct {
	Message MessageData `json:"message"`
}

// HistoryData — тело ответа GET /messages (см. rest.go).
type HistoryData struct {
	Channel  string        `json:"channel"`
	Messages []MessageData `json:"messages"` // хронологически, по возрастанию id
}
type ErrorData struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
type AuthedUser struct {
	ID        int64  `json:"id"`                   // внутренний id аккаунта
	Username  string `json:"username,omitempty"`   // @username — ссылка на профиль
	Name      string `json:"name,omitempty"`       // отображаемое имя (единственное для UI)
	AvatarURL string `json:"avatar_url,omitempty"` // URL фото профиля; пусто у входа через Apple
	// Providers — какие способы входа привязаны к аккаунту (`telegram`, `apple`,
	// `google`), отсортированы. Клиенту нужны, чтобы показать кнопку привязки
	// только для непривязанного провайдера.
	Providers []string `json:"providers,omitempty"`
	// Blocked — внутренние id тех, кого этот пользователь заблокировал. Историю
	// сервер фильтрует сам, а живую ленту прячет клиент, и для этого ему нужен
	// список. Уходит только владельцу аккаунта: AuthedUser другим не показывается.
	Blocked []int64 `json:"blocked,omitempty"`
	// VotesLeft — сколько голосов у человека осталось из voteBudget. Приезжает со
	// входом и с resume, дальше клиент обновляет его по ответам POST /vote.
	// Единственное число про голоса, которое человек видит: это его собственный
	// ресурс, а не оценка. Накопленного рейтинга нет в протоколе вовсе, ни для
	// владельца, ни для других: оценка конкретной реплики — часть разговора, а
	// счёт человека — табло, за которым имеет смысл гнаться.
	VotesLeft int `json:"votes_left"`
}

// AuthedData — общий шейп REST-ответов про личность: POST /auth/{provider},
// POST /session/resume и POST /rules/accept (см. rest.go). В resume/accept_rules
// поле Token пустое — клиент и так прислал его в запросе; заполнено оно только в
// ответе /auth/{provider} (новая сессия).
type AuthedData struct {
	User AuthedUser `json:"user"`
	// сессионный токен: клиент сохраняет его и предъявляет в REST /session/resume
	// после реконнекта и в query ?token= при открытии WS; пустой — вне /auth/telegram
	Token string `json:"token,omitempty"`
	// принимал ли этот аккаунт правила Эфира раньше (POST /rules/accept) —
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
