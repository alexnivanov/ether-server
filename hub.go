package main

import "log/slog"

// subscribers — множество подписчиков канала (set-as-map: значение-заглушка
// не читается).
type subscribers map[*Client]bool

// Hub владеет всеми подписками каналов и рассылает сообщения подписчикам.
// Всё состояние меняется из одной горутины (Run) — клиенты общаются с ним через
// каналы, поэтому блокировки не нужны.
type Hub struct {
	// channelID → множество подписанных клиентов
	channels map[string]subscribers

	unregister chan *Client
	subscribe  chan subscription
	broadcast  chan MessageData

	// announce — кадр всем подключённым, независимо от каналов. Нужен модерации:
	// удалённое сообщение должно исчезнуть из открытых лент сразу, а не после
	// перезапуска приложения. Рассылаем всем, а не подписчикам канала, потому
	// что при удалении аккаунта сообщения могли быть в разных каналах, а
	// событие редкое — экономить тут нечего, и лишний кадр клиент просто не
	// найдёт у себя в ленте.
	announce chan Envelope
}

type subscription struct {
	client   *Client
	channels []string
}

func NewHub() *Hub {
	return &Hub{
		channels:   make(map[string]subscribers),
		unregister: make(chan *Client),
		subscribe:  make(chan subscription),
		broadcast:  make(chan MessageData),
		announce:   make(chan Envelope),
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
					h.channels[id] = make(subscribers)
				}
				h.channels[id][s.client] = true
			}

		case env := <-h.announce:
			// один клиент может быть подписан на несколько каналов — иначе он
			// получил бы кадр по разу на канал
			seen := make(map[*Client]bool)
			for _, subs := range h.channels {
				for c := range subs {
					if seen[c] {
						continue
					}
					seen[c] = true
					select {
					case c.send <- env:
					default:
						slog.Warn("send buffer full, dropping announce", "client", c.DisplayName())
					}
				}
			}

		case m := <-h.broadcast:
			env := envelope(TypeMessage, m)
			for c := range h.channels[m.Channel] {
				select {
				case c.send <- env:
				default:
					// медленный клиент: не блокируем хаб, роняем сообщение
					slog.Warn("send buffer full, dropping message", "client", c.DisplayName(), "channel", m.Channel)
				}
			}
		}
	}
}
