package main

import (
	"encoding/json"
	"log"
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
	tg    *TelegramAuth
	store *Store

	// кто вошёл: читает readPump (publish), а пишет ещё и горутина Telegram-бота
	mu     sync.Mutex
	userID int64 // Telegram user id
	nick   string
	authed bool
}

func (c *Client) Nick() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.nick
}

// authedUser отдаёт автора для publish: tg id, ник и флаг «вход выполнен».
func (c *Client) authedUser() (int64, string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.userID, c.nick, c.authed
}

func (c *Client) setAuthed(userID int64, nick string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.userID = userID
	c.nick = nick
	c.authed = true
}

func (c *Client) readPump() {
	defer func() {
		c.tg.Cancel(c) // до unregister: confirm не должен писать в закрытый send
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
		case TypeLoginTelegram:
			c.out(envelope(TypeLoginLink, LoginLinkData{URL: c.tg.NewLoginToken(c)}))

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
			userID, nick, authed := c.authedUser()
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
			m := MessageData{
				Channel: d.Channel,
				Sender:  nick,
				Text:    d.Text,
				TS:      time.Now().UnixMilli(),
			}
			if id, err := c.store.SaveMessage(m.Channel, userID, m.Sender, m.Text, m.TS); err != nil {
				log.Printf("save message: %v", err) // живая рассылка важнее истории
			} else {
				m.ID = id
			}
			c.hub.broadcast <- m

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
		log.Printf("send buffer full for %q, dropping %s", c.Nick(), env.Type)
	}
}

func (c *Client) sendError(code, msg string) {
	c.out(envelope(TypeError, ErrorData{Code: code, Message: msg}))
}
