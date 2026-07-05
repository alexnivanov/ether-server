package main

import "log"

// Hub владеет всеми подписками каналов и рассылает сообщения подписчикам.
// Всё состояние меняется из одной горутины (Run) — клиенты общаются с ним через
// каналы, поэтому блокировки не нужны.
type Hub struct {
	// channelID → множество подписанных клиентов
	channels map[string]map[*Client]bool
	// все живые соединения — чтобы направленная доставка (direct) была
	// сериализована с close(send) в unregister
	clients map[*Client]bool

	register   chan *Client
	unregister chan *Client
	subscribe  chan subscription
	broadcast  chan MessageData
	direct     chan directEnvelope
}

type subscription struct {
	client   *Client
	channels []string
}

// directEnvelope — кадр одному конкретному соединению (например authed из
// горутины Telegram-бота).
type directEnvelope struct {
	client *Client
	env    Envelope
}

func NewHub() *Hub {
	return &Hub{
		channels:   make(map[string]map[*Client]bool),
		clients:    make(map[*Client]bool),
		register:   make(chan *Client),
		unregister: make(chan *Client),
		subscribe:  make(chan subscription),
		broadcast:  make(chan MessageData),
		direct:     make(chan directEnvelope),
	}
}

func (h *Hub) Run() {
	for {
		select {
		case c := <-h.register:
			h.clients[c] = true

		case c := <-h.unregister:
			delete(h.clients, c)
			for id, subs := range h.channels {
				if subs[c] {
					delete(subs, c)
					if len(subs) == 0 {
						delete(h.channels, id)
					}
				}
			}
			close(c.send)

		case d := <-h.direct:
			if h.clients[d.client] {
				select {
				case d.client.send <- d.env:
				default:
					log.Printf("send buffer full, dropping %s", d.env.Type)
				}
			}

		case s := <-h.subscribe:
			for _, id := range s.channels {
				if h.channels[id] == nil {
					h.channels[id] = make(map[*Client]bool)
				}
				h.channels[id][s.client] = true
			}

		case m := <-h.broadcast:
			env := envelope(TypeMessage, m)
			for c := range h.channels[m.Channel] {
				select {
				case c.send <- env:
				default:
					// медленный клиент: не блокируем хаб, роняем сообщение
					log.Printf("send buffer full for %q, dropping message in %s", c.Nick(), m.Channel)
				}
			}
		}
	}
}
