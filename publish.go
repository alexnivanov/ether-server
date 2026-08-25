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
		scope := scopeForChannel(channel)
		if ok, retry := p.limiter.Allow(a.ID, 0, a.AccountAge, scope); !ok {
			return MessageData{}, &publishError{
				Code:    "too_fast",
				Message: tooFastMessage(scope, retry),
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

// scopeForChannel — из какого запаса брать сообщение. Знание про форму ID живёт
// здесь, а не в ratelimit.go: там про каналы не знают, там арифметика бакетов.
// Никакого геокодинга — только вид строки, по контракту ID каналов
// (ether-meta/CLAUDE.md):
//
//	EARTH             — планета, зарезервированный литерал (см. PlanetChannel)
//	RU                — страна, ISO 3166-1: ровно две заглавные буквы
//	RU-MOW            — область, ISO 3166-2: с дефисом, лимита нет
//	relation/2555133  — город/район/квартал: неразличимы, лимита нет
//
// Всё, что в эти формы не попало (мусорная строка от клиента, канал в неизвестном
// формате), считается локальным — то есть ведёт себя как сегодня.
func scopeForChannel(channel string) limitScope {
	if channel == PlanetChannel.ID {
		return scopePlanet
	}
	if isCountryCode(channel) {
		return scopeCountry
	}
	return scopeLocal
}

// isCountryCode — ID страны по контракту: ровно две заглавные латинские буквы.
// Регистр не нормализуем: геокодер отдаёт код уже в верхнем (strings.ToUpper над
// country_code в nominatim.go), а «ru» с клиента — это не канал страны, а
// мусорная строка, и поблажки ей не нужны.
func isCountryCode(s string) bool {
	if len(s) != 2 {
		return false
	}
	return s[0] >= 'A' && s[0] <= 'Z' && s[1] >= 'A' && s[1] <= 'Z'
}

// tooFastMessage — текст отказа. Код остаётся too_fast для всех скоупов, менять
// его нельзя: у сборок в сторах ветка «модальный диалог + вернуть текст в поле»
// заведена на литералы too_fast и banned, а на незнакомом коде человек получит
// снекбар и потеряет написанное (см. ether-meta/PLANS.md). Так что меняется
// только формулировка — и меняется по делу: «подожди 3421 с» на часовом лимите
// не читается, а причина отказа тут не «частишь», а «чем шире канал, тем реже в
// нём пишут».
func tooFastMessage(scope limitScope, retry time.Duration) string {
	switch scope {
	case scopePlanet:
		return fmt.Sprintf("В Землю можно писать раз в час — следующее сообщение через %s", humanWait(retry))
	case scopeCountry:
		return fmt.Sprintf("В канале страны пишут реже — подожди %s", humanWait(retry))
	default:
		return fmt.Sprintf("Слишком часто — подожди %d с", int(retry.Seconds())+1)
	}
}

// humanWait — ожидание словами. Округляем ВВЕРХ: по подсказке «через 46 мин»
// человек вернётся ровно к сроку, и отказ второй раз он получить не должен.
func humanWait(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d с", int(d.Seconds())+1)
	case d < time.Hour:
		return fmt.Sprintf("%d мин", int((d+time.Minute-1)/time.Minute))
	default:
		return fmt.Sprintf("%d ч", int((d+time.Hour-1)/time.Hour))
	}
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
