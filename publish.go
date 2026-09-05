package main

import (
	"fmt"
	"log/slog"
	"time"
)

// Публикация сообщения — путь `POST /messages` (rest.go) целиком: проверка
// бана, лимит частоты, запись в историю, рассылка подписчикам и пуш тем, кого
// нет в сокете.
//
// Отдельный файл, а не тело хендлера: транспорт — это способ доставки запроса,
// а не логика сообщения. Раньше транспортов было два (кадр `publish` на WS и
// запрос), и общая функция держала их проверки едиными: разъехавшись, они дали
// бы «через один транспорт забаненный писать не может, а через другой может», и
// заметить такое можно было только жалобой. Кадр снят, разделение осталось —
// оно и держит границу между HTTP и тем, что значит «опубликовать».

// publishError — отказ публикации в терминах протокола: код и текст для
// человека плюс HTTP-статус. Retry заполняется только у too_fast — он
// становится заголовком Retry-After.
type publishError struct {
	Code    string
	Message string
	Status  int
	Retry   time.Duration
}

func (e *publishError) Error() string { return e.Code + ": " + e.Message }

// publishAuthor — кто публикует. Имя, @username и аватар читаются из сессии
// запроса, а в историю уходит только внутренний id автора (см.
// Store.SaveMessage): эти поля нужны для live-рассылки, где JOIN'а нет.
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
// в ответ уходит уже сохранённое. Пустой id идемпотентности не даёт — поле
// необязательное.
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
	// Частота публикаций. Рейтинг — сумма голосов за сообщения автора за
	// последнюю неделю (голоса уезжают вместе с сообщениями), ещё один SELECT по
	// индексу рядом с проверкой бана. Ошибку не считаем отказом: без рейтинга
	// работает базовый тир, и это лучше, чем не дать написать из-за сбоя чтения.
	if p.limiter != nil {
		rating, err := p.store.AuthorRating(a.ID)
		if err != nil {
			slog.Error("author rating", "err", err, "user_id", a.ID)
		}
		scope := p.scopeFor(channel)
		if ok, retry := p.limiter.Allow(a.ID, rating, a.AccountAge, scope); !ok {
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

// scopeFor — из какого запаса брать сообщение. Полосу канала берём из
// справочника (channels.level, заполняется на каждый locate нашим же
// геокодером): по форме ID город неотличим от района и квартала, а уровню от
// клиента верить нельзя — пришлёт `quarter` и получит квартальный темп в
// городском канале.
//
// Справочник может молчать: строки нет у баз, живших до появления таблицы, и до
// первого locate в этом месте. Тогда падаем на разбор формы ID
// (scopeForChannel) — он различает верхние полосы, а остальное считает локальным.
// Отказывать в публикации из-за отсутствия справочной строки нельзя: сообщение
// потерялось бы на пустом месте.
func (p *publisher) scopeFor(channel string) limitScope {
	level, err := p.store.ChannelLevel(channel)
	if err != nil {
		slog.Error("channel level", "err", err, "channel", channel)
	}
	if scope, ok := scopeForLevel(level); ok {
		return scope
	}
	return scopeForChannel(channel)
}

// scopeForLevel — полоса по уровню из справочника. Значения — те, что пишет
// геокодер (см. Channel.Level): planet | country | region | city | district |
// quarter. Неизвестное или пустое — false, вызывающий решает сам.
func scopeForLevel(level string) (limitScope, bool) {
	switch level {
	case "planet":
		return scopePlanet, true
	case "country":
		return scopeCountry, true
	case "region", "city":
		return scopeCity, true
	case "district", "quarter":
		return scopeLocal, true
	}
	return scopeLocal, false
}

// scopeForChannel — запасной разбор по ВИДУ строки, когда справочник про канал
// молчит. Никакого геокодинга, только форма ID по контракту
// (ether-meta/CLAUDE.md):
//
//	EARTH             — планета, зарезервированный литерал (см. PlanetChannel)
//	RU                — страна, ISO 3166-1: ровно две заглавные буквы
//	RU-MOW            — область, ISO 3166-2: две буквы, дефис, код субъекта
//	relation/2555133  — город/район/квартал: по форме неразличимы, считаем
//	                    локальным (город без справочника опознать нельзя)
//
// Всё, что в эти формы не попало (мусорная строка от клиента, канал в неизвестном
// формате), тоже локальное — то есть ведёт себя как самая мягкая полоса.
func scopeForChannel(channel string) limitScope {
	if channel == PlanetChannel.ID {
		return scopePlanet
	}
	if isCountryCode(channel) {
		return scopeCountry
	}
	if isRegionCode(channel) {
		return scopeCity
	}
	return scopeLocal
}

// isRegionCode — ID области по контракту: ISO 3166-2, то есть код страны, дефис
// и код субъекта («RU-MOW», «DE-HE»). Регистр, как и у страны, не нормализуем:
// геокодер отдаёт значение из OSM как есть, а самодельная строка каналом не
// является.
func isRegionCode(s string) bool {
	if len(s) < 4 || s[2] != '-' {
		return false
	}
	if !isCountryCode(s[:2]) {
		return false
	}
	for i := 3; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			continue
		}
		return false
	}
	return true
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
// заведена на литералы too_fast и banned (см. _publish в
// ether-client/lib/main.dart), а на незнакомом коде человек получит снекбар и
// потеряет написанное. Так что меняется
// только формулировка — и меняется по делу: «подожди 3421 с» на часовом лимите
// не читается, а причина отказа тут не «частишь», а «чем шире канал, тем реже в
// нём пишут».
func tooFastMessage(scope limitScope, retry time.Duration) string {
	switch scope {
	case scopePlanet:
		return fmt.Sprintf("В Землю можно писать раз в час — следующее сообщение через %s", humanWait(retry))
	case scopeCountry:
		return fmt.Sprintf("В канале страны пишут реже — подожди %s", humanWait(retry))
	case scopeCity:
		return fmt.Sprintf("В городском канале пишут реже — подожди %s", humanWait(retry))
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
