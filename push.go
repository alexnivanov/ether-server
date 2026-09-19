package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// Pusher шлёт пуши о новых сообщениях через FCM HTTP v1 — **адресно, по токенам
// устройств**. Раньше модель была «топик = канал», но в топик нельзя не
// отправить конкретному подписчику, и автор получал уведомление о своём же
// сообщении; заодно fan-out по топику давал задержку в минуты на Android.
// Теперь сервер знает каналы каждого пользователя (user_channels, обновляются на
// locate) и токены его устройств (device_tokens), поэтому получателей считает сам
// и исключает автора (см. Store.PushTargets).
//
// Пуши опциональны: без service-account JSON в конфиге Pusher не создаётся
// (nil), и publish работает как раньше, просто без уведомлений.
type Pusher struct {
	projectID string
	ts        oauth2.TokenSource
	http      *http.Client
	store     *Store // нужен, чтобы вычислить получателей и убрать мёртвые токены

	// Пары «проголосовавший + сообщение», о которых автора уже будили. Голос
	// можно снять и поставить заново, и каждый раз это новая строка в votes,
	// то есть новый повод для уведомления; запас голосов качелям не помеха —
	// снятый голос возвращается в него целиком. Поэтому предел здесь.
	//
	// В памяти, а не в базе: пуш и так доставка без гарантий, а рестарт сервера
	// в худшем случае стоит автору одного лишнего уведомления. Набор ограничен
	// сверху сам: голоса не переживают сообщения, то есть неделю.
	mu       sync.Mutex
	notified map[string]struct{}
}

// NewPusher читает service-account JSON. Пустой credsFile → (nil, nil): пуши
// выключены. Ошибку чтения/парсинга возвращаем — вызывающий решает, что делать
// (в main это лог + работа без пушей, не фатал).
func NewPusher(projectID, credsFile string, store *Store) (*Pusher, error) {
	if credsFile == "" {
		return nil, nil
	}
	data, err := os.ReadFile(credsFile)
	if err != nil {
		return nil, err
	}
	creds, err := google.CredentialsFromJSON(context.Background(), data,
		"https://www.googleapis.com/auth/firebase.messaging")
	if err != nil {
		return nil, err
	}
	return &Pusher{
		projectID: projectID,
		ts:        creds.TokenSource, // сам обновляет access-token по мере протухания
		http:      &http.Client{Timeout: 10 * time.Second},
		store:     store,
		notified:  map[string]struct{}{},
	}, nil
}

// Notify рассылает уведомление о новом сообщении всем устройствам подписчиков
// канала, кроме автора. Задумана для вызова в горутине: блокируется на HTTP к
// FCM (по запросу на токен — в HTTP v1 нет мультикаста), а доставка сообщения по
// WS от пуша не зависит, поэтому ошибки только логируем.
func (p *Pusher) Notify(channelID string, senderID int64, sender, text string) {
	tokens, err := p.store.PushTargets(channelID, senderID)
	if err != nil {
		slog.Error("push targets", "err", err, "channel", channelID)
		return
	}
	if len(tokens) == 0 {
		return // некому: в канале нет других устройств
	}
	// Имя канала — из справочника (заполняется на locate, см. Store.SaveChannels):
	// в сообщении лежит только ID, а человеку нужно понять, откуда оно пришло.
	// Ошибка и пустое имя равносильны: уведомление уйдёт без названия канала.
	name := p.channelName(channelID)
	title, body := pushText(name, sender, text)
	p.deliver(pushNote{
		kind:    "message",
		channel: channelID,
		title:   title,
		body:    body,
		data:    channelData(channelID, name),
	}, tokens)
}

// NotifyVote уведомляет автора о том, что под его сообщением появилась отметка.
// Получатель ровно один — сам автор, поэтому устройства берём напрямую
// (Store.DeviceTokensForUser), а не через подписчиков канала.
//
// Знак голоса в уведомлении НЕ называется. Плюс и минус в Эфире стоят
// одинаково, а «тебя заминусовали» в шторке — это удар в спину человеку,
// который в приложение сейчас не смотрит; что именно случилось, видно по
// рейтингу, когда он его откроет.
//
// Как и Notify, задумана для вызова в горутине: блокируется на HTTP к FCM, а
// ответ на POST /vote от уведомления не зависит.
func (p *Pusher) NotifyVote(voterID, messageID int64, n VoteNotice) {
	if !p.firstVoteNotice(voterID, messageID) {
		return
	}
	tokens, err := p.store.DeviceTokensForUser(n.AuthorID)
	if err != nil {
		slog.Error("vote push targets", "err", err, "user_id", n.AuthorID)
		return
	}
	if len(tokens) == 0 {
		return // у автора нет устройств с включёнными пушами
	}
	name := p.channelName(n.Channel)
	title, body := voteText(name, n.Text)
	data := channelData(n.Channel, name)
	// type отличает уведомление об отметке от уведомления о сообщении, а
	// message_id ведёт тап к самому сообщению, а не просто в комнату.
	data["type"] = "vote"
	data["message_id"] = strconv.FormatInt(messageID, 10)
	p.deliver(pushNote{
		kind:    "vote",
		channel: n.Channel,
		title:   title,
		body:    body,
		data:    data,
	}, tokens)
}

// firstVoteNotice — «об этой паре автора ещё не будили». Второй раз по той же
// паре отвечает false, и уведомление не уходит (см. Pusher.notified).
func (p *Pusher) firstVoteNotice(voterID, messageID int64) bool {
	key := strconv.FormatInt(voterID, 10) + ":" + strconv.FormatInt(messageID, 10)
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, seen := p.notified[key]; seen {
		return false
	}
	// Чистим не по одной записи, а разом: отдельного срока жизни у ключа нет
	// (голоса уезжают вместе с сообщениями, и сообщать об этом сюда некому), а
	// сброс всей карты стоит одному автору одного лишнего уведомления.
	if len(p.notified) >= maxVoteNotices {
		clear(p.notified)
	}
	p.notified[key] = struct{}{}
	return true
}

// maxVoteNotices — потолок набора пар до сброса. 10 000 отметок — это заведомо
// больше недели жизни голосов при нынешнем размере Эфира, то есть в норме
// сброса не случается вовсе.
const maxVoteNotices = 10_000

// channelName — имя канала для шторки. Ошибку логируем и живём дальше: имя
// украшает уведомление, но не решает, слать его или нет.
func (p *Pusher) channelName(channelID string) string {
	name, err := p.store.ChannelName(channelID)
	if err != nil {
		slog.Error("channel name", "err", err, "channel", channelID)
	}
	return name
}

// pushNote — готовое уведомление: что показать и что положить в data.
type pushNote struct {
	kind    string // message | vote — только для строки в логе
	channel string
	title   string
	body    string
	data    map[string]string
}

// deliver рассылает уведомление по токенам и подчищает мёртвые. Общая часть
// Notify и NotifyVote: различаются они тем, кому и что шлют, а транспорт,
// чистка токенов и строка в логе у них одни.
func (p *Pusher) deliver(n pushNote, tokens []string) {
	tok, err := p.ts.Token()
	if err != nil {
		slog.Error("fcm token", "err", err)
		return
	}
	var sent int
	var stale []string
	for _, device := range tokens {
		switch p.sendTo(tok.AccessToken, device, n.title, n.body, n.data) {
		case sendOK:
			sent++
		case sendStale:
			// приложение удалено/токен перевыпущен — чтобы не долбить FCM зря
			stale = append(stale, device)
		}
	}
	if len(stale) > 0 {
		if err := p.store.DeleteDeviceTokens(stale); err != nil {
			slog.Error("delete stale tokens", "err", err)
		}
	}
	slog.Info("fcm send", "kind", n.kind, "channel", n.channel, "sent", sent,
		"targets", len(tokens), "stale", len(stale))
}

// pushText — что человек увидит в уведомлении. Заголовок — КАНАЛ, а не автор:
// так же устроены групповые чаты, и ОС группирует уведомления по заголовку —
// значит по каналу, а не по случайному соседу.
//
// Имя канала пустое (справочник ещё не знает этот ID, база жила до его
// появления) — остаётся прежний вид, автор в заголовке: уведомление без
// названия лучше, чем уведомление с пустой первой строкой.
func pushText(channel, sender, text string) (title, body string) {
	if channel == "" {
		return sender, text
	}
	return channel, sender + ": " + text
}

// voteText — что автор прочитает в шторке, когда его сообщение отметили.
// Заголовок тот же, что у уведомления о сообщении, — канал: так ОС сложит оба
// вида в одну стопку по зоне, а не разведёт по видам события.
//
// Фрагмент самого сообщения в теле не для красоты: у человека в канале их
// несколько, и «твоё сообщение отметили» без текста заставляет его искать, о
// каком речь. Имени проголосовавшего нет намеренно — отметка анонимна и в
// ленте (под сообщением только сумма), а в уведомлении она выдала бы соседа.
//
// Имя канала неизвестно (справочник о нём ещё не знает) — заголовком становится
// имя приложения: пустая первая строка выглядит как сбой.
func voteText(channel, text string) (title, body string) {
	title = channel
	if title == "" {
		title = "Эфир"
	}
	if r := []rune(text); len(r) > maxVoteText {
		text = string(r[:maxVoteText]) + "…"
	}
	return title, "Твоё сообщение отметили: " + text
}

// maxVoteText — сколько символов сообщения показать. Шторка всё равно обрежет
// длинное сама, но обрезать по рунам надо нам: срез по байтам развалил бы
// кириллицу пополам.
const maxVoteText = 80

// channelData — общая часть data у обоих видов уведомления: какую комнату
// открыть по тапу и как её назвать. Пустое имя не кладём вовсе — клиенту
// «поля нет» и «поле пустое» означают одно и то же, а в контракте лишнее поле
// пришлось бы объяснять.
func channelData(channelID, channelName string) map[string]string {
	data := map[string]string{"channel": channelID}
	if channelName != "" {
		data["channel_name"] = channelName
	}
	return data
}

type sendResult int

const (
	sendOK sendResult = iota
	sendFailed
	sendStale // токен больше не существует — удалить из БД
)

// pushPayload — тело запроса к FCM HTTP v1 для одного устройства.
//
// Кроме notification (то, что человек читает в шторке) кладём data — то, что
// читает приложение по тапу: в какой канал открывать, а у отметки ещё и к
// какому сообщению вести. Без него тап приводил человека просто «в приложение»,
// и найти сообщение, о котором его позвали, он должен был сам.
//
// data приходит готовой (см. channelData и NotifyVote), потому что у разных
// уведомлений она разная, а собирать её по флагам внутри значило бы держать
// знание про виды уведомлений в слое транспорта. Значения у FCM всегда строки,
// отсюда map[string]string, а не any.
//
// Отдельная функция ради теста: собранный payload — это контракт с клиентом
// (см. ether-meta/PROTOCOL.md), и проверять его надо на JSON, а не на живом
// HTTP к Google.
func pushPayload(device, title, body string, data map[string]string) []byte {
	payload, _ := json.Marshal(map[string]any{
		"message": map[string]any{
			"token": device,
			"notification": map[string]any{
				"title": title,
				"body":  body,
			},
			"data": data,
		},
	})
	return payload
}

// sendTo отправляет одно уведомление на один токен устройства. title/body уже
// готовы (см. pushText), тело — pushPayload; здесь только транспорт.
func (p *Pusher) sendTo(accessToken, device, title, body string, data map[string]string) sendResult {
	payload := pushPayload(device, title, body, data)
	url := fmt.Sprintf("https://fcm.googleapis.com/v1/projects/%s/messages:send", p.projectID)
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.http.Do(req)
	if err != nil {
		slog.Error("fcm send", "err", err)
		return sendFailed
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return sendOK
	}
	b, _ := io.ReadAll(resp.Body)
	reason := string(b)
	// 404 UNREGISTERED / 400 с невалидным токеном — устройство больше не наше
	if resp.StatusCode == http.StatusNotFound || strings.Contains(reason, "UNREGISTERED") ||
		strings.Contains(reason, "INVALID_ARGUMENT") {
		slog.Info("fcm token stale", "status", resp.Status)
		return sendStale
	}
	slog.Warn("fcm send rejected", "status", resp.Status, "body", reason)
	return sendFailed
}
