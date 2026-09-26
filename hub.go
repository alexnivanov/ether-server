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

	// announceIn — кадр подписчикам ОДНОГО канала. Отличие от announce не в
	// экономии: событие про сообщение, а сообщение живёт в канале, и незачем
	// рассказывать о нём тем, кто его не видит. announce остаётся для событий,
	// у которых канала нет вовсе (удаление всех сообщений автора).
	announceIn chan channelFrame
}

// channelFrame — кадр вместе с каналом, подписчикам которого он предназначен.
type channelFrame struct {
	channel string
	env     Envelope
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
		announceIn: make(chan channelFrame),
	}
}

// AnnounceRemoved говорит подключённым клиентам убрать контент из ленты: одно
// сообщение или всё, что написал человек. Без этого удаление выглядит для них
// так, будто ничего не произошло, — до перезапуска приложения.
//
// Путь общий у модерации (admin.go) и у автора, удаляющего своё сообщение
// (rest.go): кадр один и тот же, различается только кто его вызвал. nil-хаб
// молча ничего не делает — так удобнее тестам, которым рассылка не нужна.
func (h *Hub) AnnounceRemoved(d RemovedData) {
	if h == nil {
		return
	}
	h.announce <- envelope(TypeRemoved, d)
}

// AnnounceVoted говорит подписчикам канала, что сумма голосов под сообщением
// стала другой. nil-хаб молча ничего не делает — как и у AnnounceRemoved.
func (h *Hub) AnnounceVoted(channel string, d VotedData) {
	if h == nil {
		return
	}
	h.announceIn <- channelFrame{channel: channel, env: envelope(TypeVoted, d)}
}

// AnnounceEdited говорит подписчикам канала, что автор поправил текст
// сообщения. nil-хаб молча ничего не делает — как и у AnnounceRemoved.
func (h *Hub) AnnounceEdited(channel string, d EditedData) {
	if h == nil {
		return
	}
	h.announceIn <- channelFrame{channel: channel, env: envelope(TypeEdited, d)}
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

		case f := <-h.announceIn:
			for c := range h.channels[f.channel] {
				select {
				case c.send <- f.env:
				default:
					slog.Warn("send buffer full, dropping announce",
						"client", c.DisplayName(), "channel", f.channel)
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
