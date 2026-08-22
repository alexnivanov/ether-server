package main

import (
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	maxMessageLen   = 4096 // байт текста в publish
	maxHistoryLimit = 200  // сообщений в одном ответе history
)

// Client — одно WebSocket-соединение. readPump читает кадры из сокета и дёргает
// хаб; writePump — единственный писатель в сокет (конкурентная запись в gorilla
// запрещена), он сериализует всё исходящее из канала send.
type Client struct {
	hub   *Hub
	conn  *websocket.Conn
	send  chan Envelope
	geo   Geocoder
	store *Store
	push  *Pusher // FCM-пуши о новых сообщениях; nil — пуши выключены

	// частота публикаций (см. ratelimit.go); общий на все соединения, поэтому
	// лимит держится на аккаунт, а не на сокет — иначе его обходили бы вторым
	// подключением. nil — без ограничений (тесты).
	limiter *RateLimiter

	// кто вошёл: проставляется один раз при апгрейде из ?token= (см. wsHandler),
	// дальше только читается (publish)
	mu        sync.Mutex
	userID    int64     // внутренний id аккаунта
	fullName  string    // отображаемое имя автора — в live-сообщения
	username  string    // @username — в live-сообщения для ссылки на профиль
	avatarURL string    // фото профиля — в live-сообщения автора
	createdAt time.Time // когда аккаунт зарегистрирован — для лимита свежих
	authed    bool
}

func (c *Client) DisplayName() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fullName
}

// author отдаёт данные автора для publish: внутренний id, имя, @username,
// аватар и флаг «вход выполнен».
func (c *Client) author() (id int64, name, username, avatar string, authed bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.userID, c.fullName, c.username, c.avatarURL, c.authed
}

// publisher собирает общий путь публикации из того, что уже есть у соединения
// (см. publish.go). Отдельного поля не держим: publisher — это склейка четырёх
// зависимостей, а не состояние.
func (c *Client) publisher() *publisher {
	return &publisher{store: c.store, hub: c.hub, push: c.push, limiter: c.limiter}
}

// accountAge — сколько живёт аккаунт. Считается от сохранённой даты
// регистрации, поэтому свежий аккаунт «дорастает» до обычного лимита прямо на
// открытом соединении, без переподключения.
func (c *Client) accountAge() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.createdAt.IsZero() {
		return 0
	}
	return time.Since(c.createdAt)
}

func (c *Client) setAuthed(userID int64, fullName, username, avatarURL string, createdAt time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.userID = userID
	c.fullName = fullName
	c.username = username
	c.avatarURL = avatarURL
	c.createdAt = createdAt
	c.authed = true
}

func (c *Client) readPump() {
	defer func() {
		c.hub.unregister <- c
		c.conn.Close()
	}()

	for {
		_, raw, err := c.conn.ReadMessage()
		if err != nil {
			return
		}
		var env Envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			c.sendError("bad_json", "cannot parse envelope")
			continue
		}

		switch env.Type {
		case TypeLocate:
			var d LocateData
			if err := json.Unmarshal(env.Data, &d); err != nil {
				c.sendError("bad_data", "invalid locate payload")
				continue
			}
			chans, err := c.geo.Channels(d.Lat, d.Lng)
			if err != nil {
				c.sendError("geocode_failed", err.Error())
				continue
			}
			ids := make([]string, 0, len(chans))
			for _, ch := range chans {
				ids = append(ids, ch.ID)
			}
			c.hub.subscribe <- subscription{client: c, channels: ids}
			// Каналы вошедшего запоминаем в БД — по ним считаются получатели
			// пушей, когда сокета уже нет (см. Store.PushTargets). Полная
			// замена набора: уехал из района — пуши оттуда прекращаются.
			//
			// Порядок важен: сначала записываем свои каналы, потом считаем
			// подписчиков — иначе человек не увидел бы в счётчике себя.
			if userID, _, _, _, authed := c.author(); authed {
				if err := c.store.SetUserChannels(userID, ids); err != nil {
					slog.Error("set user channels", "err", err, "user_id", userID)
				}
			}
			// Число подписчиков заполняем ЗДЕСЬ, а не в геокодере: тот про
			// географию и его ответы кешируются на сутки, а счётчик живой.
			// Сбой запроса не повод рушить locate — покажем нули.
			if counts, err := c.store.ChannelSubscribers(ids); err != nil {
				slog.Error("channel subscribers", "err", err)
			} else {
				for i := range chans {
					chans[i].Subscribers = counts[chans[i].ID]
				}
			}
			c.out(envelope(TypeLocated, LocatedData{Channels: chans}))

		// Кадр `publish` живёт только ради сборок, которые не умеют
		// POST /messages (см. ether-meta/PLANS.md, шаг 3). Сама публикация — общая
		// с REST функция, здесь остаётся только разбор кадра и ответ ошибкой.
		case TypePublish:
			userID, name, username, avatar, authed := c.author()
			if !authed {
				c.sendError("not_authed", "отправка доступна после входа через Telegram")
				continue
			}
			var d PublishData
			if err := json.Unmarshal(env.Data, &d); err != nil {
				c.sendError("bad_data", "invalid publish payload")
				continue
			}
			author := publishAuthor{
				ID:         userID,
				Name:       name,
				Username:   username,
				AvatarURL:  avatar,
				AccountAge: c.accountAge(),
			}
			// client_msg_id на WS не передаётся: кадр отправляется в буфер и
			// подтверждения не имеет, поэтому повторить его клиент всё равно не
			// может — идемпотентность защищать нечего (это и есть та причина, по
			// которой отправка уезжает в REST).
			// Сохранённое сообщение здесь не нужно: автор получит его рассылкой
			// из publish, тем же кадром `message` и в том же порядке, что чужие.
			if _, perr := c.publisher().publish(
				author, d.Channel, d.Text, ""); perr != nil {
				c.sendError(perr.Code, perr.Message)
				continue
			}

		default:
			c.sendError("unknown_type", "unknown message type: "+env.Type)
		}
	}
}

func (c *Client) writePump() {
	for env := range c.send {
		if err := c.conn.WriteJSON(env); err != nil {
			return
		}
	}
}

// out кладёт кадр в очередь на отправку, не блокируя вызывающую горутину.
func (c *Client) out(env Envelope) {
	select {
	case c.send <- env:
	default:
		slog.Warn("send buffer full, dropping frame", "client", c.DisplayName(), "type", env.Type)
	}
}

func (c *Client) sendError(code, msg string) {
	c.out(envelope(TypeError, ErrorData{Code: code, Message: msg}))
}
