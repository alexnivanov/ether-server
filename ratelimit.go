package main

import (
	"sync"
	"time"
)

// Ограничение частоты публикаций. Нужно и само по себе (публичный гео-чат без
// него зафлудят в первый день, и клиент с retry-циклом тоже), и как пол под
// рейтинг пользователей: лимит выбирается по репутации, а нулевая репутация —
// это и есть базовый («плоский») лимит. Голоса уже есть (см. Store.Vote), но
// рейтинг у большинства остаётся нулевым, то есть базовый тир — рабочий, а не
// теоретический случай.
//
// Алгоритм — токен-бакет на аккаунт: capacity задаёт всплеск (сколько сообщений
// можно отправить подряд), refill — устойчивый темп. Так живой человек, который
// быстро дописывает мысль в три сообщения, не спотыкается, а поток без паузы
// упирается в темп.
//
// Состояние в памяти, не в БД: лимит — защита от потока «здесь и сейчас»,
// переживать рестарт ему незачем, а запись в SQLite на каждое сообщение при
// одном писателе только мешала бы. Для часового тира Земли это значит, что
// деплой выдаёт всем по одному дополнительному сообщению — принято осознанно:
// деплои редкие, а точный вариант (считать последнее сообщение автора в EARTH
// по истории, TTL там 7 дней) требует индекса по (user_id, channel), то есть
// миграции ради погрешности в одно сообщение.

// messageLimit — параметры тира.
type messageLimit struct {
	capacity int           // всплеск: сколько сообщений подряд без ожидания
	refill   time.Duration // сколько ждать восстановления одного сообщения
}

// limitScope — по какому бакету считать сообщение. Скоупы делятся не по уровням
// админделения, а по **цене охвата**: Земля есть у всех, поэтому реплика туда
// стоит дороже любой локальной, и считать их вместе нельзя — иначе одно
// сообщение на весь мир на час затыкало бы человека в его же квартале.
//
// Полоса канала приезжает из справочника (channels.level, см.
// publisher.scopeFor), поэтому лестница дотягивается до города. Две склейки в
// ней намеренные:
//
//	область + город — по числу людей сопоставимы; ограничить область, оставив
//	  город свободным, значило бы наказать за уровень, а не за охват;
//	район + квартал — различие некуда потратить (внизу базовый темп, который
//	  живой человек и так не выбирает), а метка quarter вдобавок относительная,
//	  см. Store.ChannelLevel.
type limitScope int

const (
	scopeLocal   limitScope = iota // район и квартал — один бакет на аккаунт
	scopePlanet                    // канал Земля
	scopeCountry                   // канал страны
	scopeCity                      // область и город
)

// Земля: одно сообщение в час при нулевом рейтинге, всплеска нет. Каждый минус
// добавляет к паузе полчаса, но не дальше суток; каждый плюс делит паузу
// пополам, но не быстрее раза в минуту.
//
// Тиров здесь нет: при ступенях −1 и −30 давали одно и то же, а первый же минус
// от одного человека удваивал паузу. По формуле одиночный минус почти незаметен,
// а массовое неодобрение весит больше единичного. Потолок нужен по тому же
// принципу, что и везде: минус не затыкает совсем, а замедляет.
//
// Пол в минуту наступает уже на +6; почему его не выкручивают накруткой, см.
// planetRating.
const (
	planetRefill     = time.Hour
	planetPenalty    = 30 * time.Minute // за каждый минус рейтинга
	planetMaxRefill  = 24 * time.Hour
	planetMaxPenalty = int((planetMaxRefill - planetRefill) / planetPenalty)
	planetMinRefill  = time.Minute
)

// planetRating — рейтинг, по которому считается тир Земли: торможение берётся
// из общего рейтинга, разгон — только из голосов под сообщениями в самой Земле.
// Иначе плюсы, собранные в районе (там 20 сообщений в минуту), разгоняли бы
// Землю за секунды. В Земле же каждый плюс от одного человека требует нового
// сообщения, а оно выходит не чаще текущей паузы и видно всей Земле — успеют и
// заминусовать. Общий минус перебивает плюсы в Земле: токсичность в районе не
// должна разгонять на весь мир.
func planetRating(total, planet int) int {
	if total < 0 {
		return total
	}
	return max(planet, 0)
}

// planetLimit — тир Земли при рейтинге из planetRating.
func planetLimit(rating int) messageLimit {
	refill := planetRefill
	if rating < 0 {
		refill += time.Duration(min(-rating, planetMaxPenalty)) * planetPenalty
	}
	for i := 0; i < rating && refill > planetMinRefill; i++ {
		refill /= 2
	}
	return messageLimit{capacity: 1, refill: max(refill, planetMinRefill)}
}

// Страна: всплеск 2, дальше одно сообщение в пять минут; каждый минус рейтинга
// добавляет к паузе 2,5 минуты, но не дальше часа, и всплеск тогда пропадает.
//
// Канал страны видит вся страна, и при темпе раз в минуту он превращался в
// второй локальный чат. Всплеск 2 оставлен потому, что человек часто дописывает
// мысль вторым сообщением. Минус — по формуле, как в Земле, а не ступенькой:
// иначе −1 и −30 давали бы одно и то же. Плюс здесь ничего не покупает.
//
// От возраста аккаунта не зависит: 1 в 5 мин строже любого локального тира.
const (
	countryRefill     = 5 * time.Minute
	countryPenalty    = 150 * time.Second // за каждый минус рейтинга
	countryMaxRefill  = time.Hour
	countryMaxPenalty = int((countryMaxRefill - countryRefill) / countryPenalty)
)

// countryLimit — тир страны при данном рейтинге автора.
func countryLimit(rating int) messageLimit {
	if rating >= 0 {
		return messageLimit{capacity: 2, refill: countryRefill}
	}
	minuses := min(-rating, countryMaxPenalty)
	return messageLimit{capacity: 1, refill: countryRefill + time.Duration(minuses)*countryPenalty}
}

// cityLimit — тир области и города: всплеск 2, дальше одно сообщение в минуту.
// Мягче страны, потому что и охват уже; числа здесь заведомо черновые, их
// подбирать на живой статистике.
var cityLimit = messageLimit{capacity: 2, refill: time.Minute}

// wideBadLimit — отрицательный рейтинг в области и городе: втрое реже
// устойчивого темпа. Тот же принцип, что у planetLimit, — минус не затыкает
// совсем, а замедляет.
var wideBadLimit = messageLimit{capacity: 1, refill: 3 * time.Minute}

// newAccountWindow — сколько аккаунт считается свежим. Полчаса: за это время
// живой человек успевает освоиться, а спамеру приходится ждать столько же с
// каждого нового аккаунта.
const newAccountWindow = 30 * time.Minute

// messageLimitFor выбирает тир по репутации, возрасту аккаунта и полосе канала.
//
// Знаки рейтинга разведены по смыслу: **минус тормозит везде, плюс разгоняет
// только Землю** (см. planetLimit). Внизу лестницы плюсу некуда двигать —
// разницу между 20/мин и 40/мин живой человек не выбирает даже намеренно, зато
// это ровно та награда, ради которой имеет смысл накручивать; поэтому прежний
// тир «rating >= 5 → 40/мин» здесь снят.
//
// Минус при этом не затыкает совсем: в квартальном канале на десяток человек
// несколько минусов иначе закопали бы новичка, и механика стала бы оружием.
//
// Отдельный тир для свежих аккаунтов — ответ на то, что личность у любого
// провайдера стоит дёшево: Telegram-аккаунт покупается за центы, Apple ID
// заводится вручную. Раз цену личности поднять нельзя, поднимаем цену времени —
// ферма одноразовых аккаунтов перестаёт быть удобным инструментом, потому что с
// каждого можно выдать лишь несколько сообщений. На живого новичка это влияет
// слабо: всплеск в 3 сообщения подряд остаётся.
//
// Возраст важнее репутации: заслуженный тир получает тот, кто уже пробыл в
// Эфире, — иначе схема обходилась бы накруткой плюсов с тех же свежих аккаунтов.
func messageLimitFor(rating int, accountAge time.Duration, scope limitScope) messageLimit {
	switch scope {
	case scopePlanet:
		return planetLimit(rating)
	case scopeCountry:
		return countryLimit(rating)
	case scopeCity:
		if rating < 0 {
			return wideBadLimit
		}
		return cityLimit
	}
	if accountAge < newAccountWindow {
		return messageLimit{capacity: 3, refill: 10 * time.Second} // 6/мин
	}
	if rating < 0 {
		return messageLimit{capacity: 2, refill: 10 * time.Second} // 6/мин
	}
	return messageLimit{capacity: 5, refill: 3 * time.Second} // 20/мин — база
}

type bucket struct {
	tokens float64
	last   time.Time
	// fullAt — когда бакет дольётся до capacity, то есть с какого момента он
	// неотличим от нового и его можно забыть (см. sweepLocked). Хранится
	// посчитанным, а не выводится в уборке: тир зависит от репутации и возраста
	// аккаунта, а их знает только Allow.
	fullAt time.Time
}

// RateLimiter — потокобезопасные бакеты по паре (пользователь, скоуп). Дёргается из readPump каждого
// соединения, поэтому под мьютексом.
type RateLimiter struct {
	mu      sync.Mutex
	buckets map[bucketKey]*bucket
	now     func() time.Time // подменяется в тестах
}

// bucketKey — у одного человека бакетов столько, сколько скоупов: локальный темп
// и Земля тратятся из разных запасов.
type bucketKey struct {
	user  int64
	scope limitScope
}

func NewRateLimiter() *RateLimiter {
	return &RateLimiter{buckets: make(map[bucketKey]*bucket), now: time.Now}
}

// Allow списывает одно сообщение у пользователя. Возвращает false и время до
// следующей возможности, если лимит исчерпан. rating — репутация автора (сумма
// голосов за его сообщения, см. Store.AuthorRating), accountAge — сколько живёт
// аккаунт (свежим тир уже), scope — полоса канала.
func (r *RateLimiter) Allow(userID int64, rating int, accountAge time.Duration, scope limitScope) (ok bool, retryAfter time.Duration) {
	lim := messageLimitFor(rating, accountAge, scope)
	now := r.now()

	r.mu.Lock()
	defer r.mu.Unlock()

	key := bucketKey{user: userID, scope: scope}
	b := r.buckets[key]
	if b == nil {
		// новый пользователь начинает с полного бакета: первое сообщение не
		// должно ждать
		b = &bucket{tokens: float64(lim.capacity), last: now}
		r.buckets[key] = b
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

	if b.tokens >= 1 {
		b.tokens--
	} else {
		// сколько ждать до одного целого токена
		retryAfter = time.Duration((1 - b.tokens) * float64(lim.refill))
	}
	b.fullAt = now.Add(time.Duration((float64(lim.capacity) - b.tokens) * float64(lim.refill)))
	r.sweepLocked(now)
	return retryAfter == 0, retryAfter
}

// sweepLocked выбрасывает бакеты, которые уже долились до capacity: такой бакет
// ничем не отличается от нового, поэтому хранить его незачем, а без уборки map
// рос бы вместе с числом когда-либо писавших. Зовётся из Allow под мьютексом —
// отдельная горутина-уборщик тут не нужна: проход дешёвый, а map трогается
// только когда кто-то пишет.
//
// Смотрим на fullAt, а не на «давно не писал»: при фиксированном простое
// (было 10 минут) часовой лимит Земли превращался в лимит на этот простой —
// пустой бакет удалялся, а новый создаётся ПОЛНЫМ, и следующее сообщение
// проходило. С базовым тиром (refill 3 с) разницы не было, поэтому подмена и
// выглядела безобидной: за 10 минут молчания бакет и так полон.
func (r *RateLimiter) sweepLocked(now time.Time) {
	if len(r.buckets) < 64 { // на малых объёмах не тратим время на обход
		return
	}
	for id, b := range r.buckets {
		if !now.Before(b.fullAt) {
			delete(r.buckets, id)
		}
	}
}

// ── Лимит по адресу ──────────────────────────────────────────────────────────

// geocodeIPLimit — сколько геокодингов отдаём одному адресу (GET /geocode).
// Эндпоинт публичный и без авторизации, а за ним стоит внешний Nominatim с
// лимитом 1 req/s, поэтому считать некому, кроме как по адресу.
//
// Числа под лендинг: страница тратит ОДИН запрос на визит, так что всплеск в
// десять — это запас для офиса или дома за общим NAT, где адрес у всех один.
// Устойчивый темп 4/мин отсекает перебор координат сеткой, ради которого
// эндпоинт и пришлось бы иначе закрывать.
var geocodeIPLimit = messageLimit{capacity: 10, refill: 15 * time.Second}

// ipLimiter — те же бакеты, что у RateLimiter, но ключ — адрес, а тир один на
// всех. Отдельный тип, а не скоуп в RateLimiter: там ключ по аккаунту (int64) и
// тир зависит от репутации, а здесь ни аккаунта, ни репутации нет — спрашивает
// аноним.
type ipLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	lim     messageLimit
	now     func() time.Time // подменяется в тестах
}

func newIPLimiter(lim messageLimit) *ipLimiter {
	return &ipLimiter{buckets: make(map[string]*bucket), lim: lim, now: time.Now}
}

// Allow списывает один запрос у адреса. Возвращает false и время до следующей
// возможности, если бакет пуст.
func (l *ipLimiter) Allow(ip string) (ok bool, retryAfter time.Duration) {
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()

	b := l.buckets[ip]
	if b == nil {
		// новый адрес начинает с полного бакета: первый запрос не ждёт
		b = &bucket{tokens: float64(l.lim.capacity), last: now}
		l.buckets[ip] = b
	} else {
		b.tokens += now.Sub(b.last).Seconds() / l.lim.refill.Seconds()
		if b.tokens > float64(l.lim.capacity) {
			b.tokens = float64(l.lim.capacity)
		}
		b.last = now
	}

	if b.tokens >= 1 {
		b.tokens--
	} else {
		retryAfter = time.Duration((1 - b.tokens) * float64(l.lim.refill))
	}
	b.fullAt = now.Add(time.Duration((float64(l.lim.capacity) - b.tokens) * float64(l.lim.refill)))
	l.sweepLocked(now)
	return retryAfter == 0, retryAfter
}

// sweepLocked — как у RateLimiter: долившийся бакет неотличим от нового, и без
// уборки map рос бы вместе с числом когда-либо заходивших адресов.
func (l *ipLimiter) sweepLocked(now time.Time) {
	if len(l.buckets) < 64 {
		return
	}
	for ip, b := range l.buckets {
		if !now.Before(b.fullAt) {
			delete(l.buckets, ip)
		}
	}
}
