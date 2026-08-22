package main

import (
	"fmt"
	"log/slog"
	"time"
)

// Публикация сообщения — один путь на два транспорта: кадр `publish` на WS
// (client.go) и `POST /messages` (rest.go). Здесь всё, что должно случиться с
// сообщением: проверка бана, лимит частоты, запись в историю, рассылка
// подписчикам и пуш тем, кого нет в сокете.
//
// Зачем общая функция, а не два похожих куска: транспорт — это способ доставки
// запроса, а не другая логика. Разъехавшиеся проверки означали бы, что через
// один транспорт забаненный писать не может, а через другой может, — и заметить
// такое можно только жалобой.
//
// Транспорта два не навсегда: WS-`publish` жив, пока в сторах есть сборки,
// которые не умеют REST (см. ether-meta/PLANS.md, шаг 3).

// publishError — отказ публикации в терминах протокола. Один тип на два
// транспорта: на WS уходит кадром `error` (code + message), в REST — телом
// ошибки с HTTP-статусом. Retry заполняется только у too_fast — в REST он
// становится заголовком Retry-After.
type publishError struct {
	Code    string
	Message string
	Status  int
	Retry   time.Duration
}

func (e *publishError) Error() string { return e.Code + ": " + e.Message }

// publishAuthor — кто публикует. Имя, @username и аватар берутся не из БД, а от
// вызывающего: на WS они лежат в соединении, в REST читаются из сессии. В
// историю всё равно уходит только внутренний id автора (см. Store.SaveMessage),
// эти поля нужны для live-рассылки.
type publishAuthor struct {
	ID         int64
	Name       string
	Username   string
	AvatarURL  string
	AccountAge time.Duration // для тира лимита частоты (свежие аккаунты — уже)
}

// publisher — то, что нужно публикации, кроме самого сообщения. push и limiter
// могут быть nil (пуши выключены в конфиге; в тестах — лимит не нужен).
type publisher struct {
	store   *Store
	hub     *Hub
	push    *Pusher
	limiter *RateLimiter
}

// publish проводит сообщение целиком и возвращает то, что клиент увидит в ленте.
//
// clientMsgID — идемпотентность повтора: если сообщение с этой парой
// (автор, id) уже сохранено, второй раз оно не публикуется и не рассылается, а
// в ответ уходит уже сохранённое. Пустой id (так шлёт WS-кадр — старые сборки
// про него не знают) идемпотентности не даёт.
func (p *publisher) publish(
	a publishAuthor, channel, text, clientMsgID string,
) (MessageData, *publishError) {
	if channel == "" {
		return MessageData{}, &publishError{
			Code: "bad_data", Message: "нужен канал", Status: 400,
		}
	}
	if text == "" || len(text) > maxMessageLen {
		return MessageData{}, &publishError{
			Code:    "bad_data",
			Message: fmt.Sprintf("текст должен быть от 1 до %d байт", maxMessageLen),
			Status:  400,
		}
	}
	// Повтор ищем ПЕРВЫМ делом, до бана и лимита частоты. Повторный запрос — не
	// новая публикация, а вопрос «дошло ли предыдущий раз»: сообщение уже
	// сохранено и разослано, и отвечать на такой вопрос отказом (429, 403) —
	// врать. Заодно повтор не тратит токен лимитера, иначе клиент, не получивший
	// ответ по таймауту, за свою же настойчивость получал бы «слишком часто».
	if dup, err := p.store.MessageByClientMsgID(a.ID, clientMsgID); err != nil {
		slog.Error("lookup client_msg_id", "err", err, "user_id", a.ID)
	} else if dup.exists {
		return storedMessage(a, dup), nil
	}
	// Бан мог прилететь при уже открытом сокете (BanEscalate отзывает сессии, но
	// живое соединение остаётся authed в памяти) или между двумя REST-запросами.
	// Запрос на каждую публикацию — один SELECT к локальной SQLite; на нашем
	// масштабе дешевле, чем индексировать соединения по пользователю.
	if banned, until, permanent, reason, err := p.store.BanStatus(a.ID); err != nil {
		slog.Error("ban check", "err", err, "user_id", a.ID)
	} else if banned {
		return MessageData{}, &publishError{
			Code: "banned", Message: BanMessage(until, permanent, reason), Status: 403,
		}
	}
	// Частота публикаций. rating пока всегда 0 (голосов нет) — работает базовый
	// тир; когда появится рейтинг, сюда придёт его значение и лимит станет тирным
	// без правок здесь (см. ratelimit.go).
	if p.limiter != nil {
		if ok, retry := p.limiter.Allow(a.ID, 0, a.AccountAge); !ok {
			return MessageData{}, &publishError{
				Code:    "too_fast",
				Message: fmt.Sprintf("Слишком часто — подожди %d с", int(retry.Seconds())+1),
				Status:  429,
				Retry:   retry,
			}
		}
	}

	m := MessageData{
		Channel:   channel,
		SenderID:  a.ID,
		Sender:    a.Name,
		Username:  a.Username,
		AvatarURL: a.AvatarURL,
		Text:      text,
		TS:        time.Now().UnixMilli(),
	}
	id, dup, err := p.store.SaveMessage(m.Channel, a.ID, m.Text, m.TS, clientMsgID)
	switch {
	case err != nil:
		// живая рассылка важнее истории: сообщение уйдёт подписчикам без id
		slog.Error("save message", "err", err, "channel", m.Channel)
	case dup.exists:
		// Гонка: два одинаковых запроса пришли одновременно, оба не нашли строку
		// поиском выше, и один из них уткнулся в уникальный индекс. Сообщение уже
		// в истории и уже разослано — отдаём сохранённое.
		return storedMessage(a, dup), nil
	default:
		m.ID = id
	}

	p.hub.broadcast <- m
	// пуш устройствам подписчиков канала, КРОМЕ автора (иначе человек получает
	// уведомление о своём же сообщении). Асинхронно: HTTP к FCM не должен
	// тормозить ни сокет, ни ответ REST.
	if p.push != nil {
		go p.push.Notify(m.Channel, a.ID, a.Name, m.Text)
	}
	return m, nil
}

// storedMessage — ответ на повтор: сообщение собирается из того, что лежит в
// базе, плюс профиль автора (в истории его нет, см. Store.SaveMessage). Ни
// рассылки, ни пуша здесь не будет: и то, и другое случилось на первой отправке.
func storedMessage(a publishAuthor, dup savedMessage) MessageData {
	return MessageData{
		ID:        dup.id,
		Channel:   dup.channel,
		SenderID:  a.ID,
		Sender:    a.Name,
		Username:  a.Username,
		AvatarURL: a.AvatarURL,
		Text:      dup.text,
		TS:        dup.ts,
	}
}
