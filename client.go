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

	// кто вошёл: проставляется один раз при апгрейде из ?token= (см. wsHandler),
	// дальше только читается (publish)
	mu        sync.Mutex
	userID    int64  // Telegram user id
	fullName  string // отображаемое имя автора — в live-сообщения
	username  string // @username — в live-сообщения для ссылки на профиль
	avatarURL string // фото профиля — в live-сообщения автора
	authed    bool
}

func (c *Client) DisplayName() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fullName
}

// author отдаёт данные автора для publish: tg id, имя, @username, аватар и
// флаг «вход выполнен».
func (c *Client) author() (id int64, name, username, avatar string, authed bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.userID, c.fullName, c.username, c.avatarURL, c.authed
}

func (c *Client) setAuthed(userID int64, fullName, username, avatarURL string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.userID = userID
	c.fullName = fullName
	c.username = username
	c.avatarURL = avatarURL
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
			c.out(envelope(TypeLocated, LocatedData{Channels: chans}))

		case TypePublish:
			userID, name, username, avatar, authed := c.author()
			if !authed {
				c.sendError("not_authed", "отправка доступна после входа через Telegram")
				continue
			}
			var d PublishData
			if err := json.Unmarshal(env.Data, &d); err != nil || d.Channel == "" {
				c.sendError("bad_data", "invalid publish payload")
				continue
			}
			if d.Text == "" || len(d.Text) > maxMessageLen {
				c.sendError("bad_data", "text must be 1..4096 bytes")
				continue
			}
			// в БД пишем только tg_id; имя/аватар для live берём из соединения,
			// для истории — JOIN из users (см. store.History)
			m := MessageData{
				Channel:   d.Channel,
				SenderID:  userID,
				Sender:    name,
				Username:  username,
				AvatarURL: avatar,
				Text:      d.Text,
				TS:        time.Now().UnixMilli(),
			}
			if id, err := c.store.SaveMessage(m.Channel, userID, m.Text, m.TS); err != nil {
				slog.Error("save message", "err", err, "channel", m.Channel) // живая рассылка важнее истории
			} else {
				m.ID = id
			}
			c.hub.broadcast <- m

			// пуш подписчикам топика канала (Район/Квартал — клиент подписан
			// только на них). Асинхронно: HTTP к FCM не должен тормозить сокет.
			if c.push != nil {
				go c.push.Notify(m.Channel, name, m.Text)
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
