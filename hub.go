package main

import "log"

// Hub владеет всеми подписками каналов и рассылает сообщения подписчикам.
// Всё состояние меняется из одной горутины (Run) — клиенты общаются с ним через
// каналы, поэтому блокировки не нужны.
type Hub struct {
	// channelID → множество подписанных клиентов
	channels map[string]map[*Client]bool

	unregister chan *Client
	subscribe  chan subscription
	broadcast  chan MessageData
}

type subscription struct {
	client   *Client
	channels []string
}

func NewHub() *Hub {
	return &Hub{
		channels:   make(map[string]map[*Client]bool),
		unregister: make(chan *Client),
		subscribe:  make(chan subscription),
		broadcast:  make(chan MessageData),
	}
}

func (h *Hub) Run() {
	for {
		select {
		case c := <-h.unregister:
			for id, subs := range h.channels {
				if subs[c] {
					delete(subs, c)
					if len(subs) == 0 {
						delete(h.channels, id)
					}
				}
			}
			close(c.send)

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
					log.Printf("send buffer full for %q, dropping message in %s", c.nick, m.Channel)
				}
			}
		}
	}
}
