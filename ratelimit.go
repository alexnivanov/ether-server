package main

import (
	"sync"
	"time"
)

// Ограничение частоты публикаций. Нужно и само по себе (публичный гео-чат без
// него зафлудят в первый день, и клиент с retry-циклом тоже), и как пол под
// будущий рейтинг пользователей: лимит выбирается по репутации, а нулевая
// репутация — это и есть базовый («плоский») лимит. Пока голосов нет, у всех
// рейтинг 0, то есть работает ровно базовый тир.
//
// Алгоритм — токен-бакет на аккаунт: capacity задаёт всплеск (сколько сообщений
// можно отправить подряд), refill — устойчивый темп. Так живой человек, который
// быстро дописывает мысль в три сообщения, не спотыкается, а поток без паузы
// упирается в темп.
//
// Состояние в памяти, не в БД: лимит — защита от потока «здесь и сейчас»,
// переживать рестарт ему незачем, а запись в SQLite на каждое сообщение при
// одном писателе только мешала бы.

// messageLimit — параметры тира.
type messageLimit struct {
	capacity int           // всплеск: сколько сообщений подряд без ожидания
	refill   time.Duration // сколько ждать восстановления одного сообщения
}

// messageLimitFor выбирает тир по репутации. Тиры **поощряющие**: плюсы
// поднимают лимит выше базового, минусы возвращают к узкому дну, но не затыкают
// совсем — иначе механика становится оружием (в квартальном канале на десяток
// человек несколько минусов закопали бы новичка).
func messageLimitFor(rating int) messageLimit {
	switch {
	case rating < 0:
		return messageLimit{capacity: 2, refill: 10 * time.Second} // 6/мин
	case rating >= 5:
		return messageLimit{capacity: 10, refill: 1500 * time.Millisecond} // 40/мин
	default:
		return messageLimit{capacity: 5, refill: 3 * time.Second} // 20/мин — база
	}
}

// bucketIdle — через сколько неактивности бакет можно забыть. Пользователь с
// полным бакетом ничем не отличается от нового, поэтому хранить его нет смысла;
// без уборки map рос бы вместе с числом когда-либо писавших.
const bucketIdle = 10 * time.Minute

type bucket struct {
	tokens float64
	last   time.Time
}

// RateLimiter — потокобезопасные бакеты по tg_id. Дёргается из readPump каждого
// соединения, поэтому под мьютексом.
type RateLimiter struct {
	mu      sync.Mutex
	buckets map[int64]*bucket
	now     func() time.Time // подменяется в тестах
}

func NewRateLimiter() *RateLimiter {
	return &RateLimiter{buckets: make(map[int64]*bucket), now: time.Now}
}

// Allow списывает одно сообщение у пользователя. Возвращает false и время до
// следующей возможности, если лимит исчерпан. rating — репутация (пока всегда 0).
func (r *RateLimiter) Allow(tgID int64, rating int) (ok bool, retryAfter time.Duration) {
	lim := messageLimitFor(rating)
	now := r.now()

	r.mu.Lock()
	defer r.mu.Unlock()

	b := r.buckets[tgID]
	if b == nil {
		// новый пользователь начинает с полного бакета: первое сообщение не
		// должно ждать
		b = &bucket{tokens: float64(lim.capacity), last: now}
		r.buckets[tgID] = b
	} else {
		// доливаем за прошедшее время; кэп — capacity текущего тира (репутация
		// могла измениться между сообщениями)
		elapsed := now.Sub(b.last)
		b.tokens += elapsed.Seconds() / lim.refill.Seconds()
		if b.tokens > float64(lim.capacity) {
			b.tokens = float64(lim.capacity)
		}
		b.last = now
	}

	if b.tokens < 1 {
		// сколько ждать до одного целого токена
		missing := 1 - b.tokens
		return false, time.Duration(missing * float64(lim.refill))
	}
	b.tokens--
	r.sweepLocked(now)
	return true, 0
}

// sweepLocked выбрасывает давно неактивные бакеты. Зовётся из Allow под
// мьютексом — отдельная горутина-уборщик тут не нужна: проход дешёвый, а
// map трогается только когда кто-то пишет.
func (r *RateLimiter) sweepLocked(now time.Time) {
	if len(r.buckets) < 64 { // на малых объёмах не тратим время на обход
		return
	}
	for id, b := range r.buckets {
		if now.Sub(b.last) > bucketIdle {
			delete(r.buckets, id)
		}
	}
}
