package main

import (
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"
)

// Кеш геокодинга. Набор каналов для точки практически не меняется во времени
// (административные границы живут годами), а публичный Nominatim ограничен
// 1 req/s — причём наш геокодер сериализует запросы, поэтому один locate
// занимает ~2.2 с монопольно. Без кеша задержка упирается в потолок уже на
// десятках активных пользователей: пять одновременных locate дают последнему
// ~11 секунд, и экран «Определяем каналы…» выглядит зависшим.
//
// Ключ — координаты, округлённые до 3 знаков: это сетка ~111 м по широте и
// ~60–110 м по долготе (зависит от широты). Для Района и Квартала такой
// точности достаточно с запасом, а совпадает она с порогом авто-трекинга на
// клиенте (200 м), поэтому едущий пользователь платит только за новые клетки.
//
// ВАЖНО: геокодим САМИ округлённые координаты, а не координаты того, кто первым
// попал в клетку. Иначе ответ для клетки зависел бы от порядка обращений (точка
// у края клетки могла бы «застолбить» соседний район), а так он — функция только
// клетки. Требование контракта «одни координаты → один набор ID» это усиливает,
// а не ослабляет.

const (
	// geocodeCacheTTL — границы меняются годами, поэтому сутки безопасны и
	// избавляют от вечного кеша (переименования, правки в OSM всё же бывают).
	geocodeCacheTTL = 24 * time.Hour
	// geocodeCacheMax — потолок записей. 20 тысяч клеток по ~100 м — это уже
	// приличный город целиком; память на запись мизерная (несколько каналов).
	geocodeCacheMax = 20000
	// как часто печатать статистику попаданий — по ней решается, нужен ли свой
	// Nominatim или хватает публичного
	geocodeStatsEvery = time.Hour
	// geocodeSlowWait — с какого ожидания считаем, что человек попал в очередь, а
	// не просто подождал сеть. Порог не про Nominatim, а про экран «Определяем
	// каналы…»: один запрос стоит ~2,2 с сам по себе, поэтому 5 с — это уже
	// «впереди кто-то есть». В сводке по нему считается WaitSlowN, в логе — WARN
	// с координатами клетки, чтобы было видно не только сколько раз, но и когда.
	geocodeSlowWait = 5 * time.Second
)

// cachedGeocoder — декоратор: отдаёт из кеша, при промахе спрашивает inner.
// Заодно склеивает одновременные запросы к одной клетке (single-flight): без
// этого десять человек, открывших приложение в одном дворе, выстроились бы в
// очередь на десять геокодингов вместо одного.
type cachedGeocoder struct {
	inner Geocoder
	ttl   time.Duration
	max   int
	now   func() time.Time // подменяется в тестах

	// stat — строка в geocode_request на каждый вызов; nil в тестах и при запуске
	// без хранилища. Считает именно декоратор: только он различает «отдали из
	// памяти», «сходили в сеть» и «дождались чужого запроса», и только он видит,
	// сколько вызывающий на это потратил.
	//
	// Зовётся ПОСЛЕ освобождения мьютекса: это запись в БД, и держать под ней
	// общий замок кеша значило бы сериализовать все геокодинги на диске.
	stat func(GeocodeRequest)

	mu       sync.Mutex
	entries  map[string]cacheEntry
	inflight map[string]*geocodeCall

	hits, misses int64
	lastStats    time.Time
}

type cacheEntry struct {
	channels []Channel
	stored   time.Time
}

// geocodeCall — идущий прямо сейчас запрос к inner; ждущие получат его результат.
type geocodeCall struct {
	done     chan struct{}
	channels []Channel
	err      error
}

func newCachedGeocoder(inner Geocoder, ttl time.Duration, max int) *cachedGeocoder {
	return &cachedGeocoder{
		inner:     inner,
		ttl:       ttl,
		max:       max,
		now:       time.Now,
		entries:   make(map[string]cacheEntry),
		inflight:  make(map[string]*geocodeCall),
		lastStats: time.Now(),
	}
}

// cacheKey округляет координаты до 3 знаков и возвращает ключ вместе с самими
// округлёнными значениями — геокодить будем именно их.
func cacheKey(lat, lng float64) (key string, rlat, rlng float64) {
	rlat = math.Round(lat*1000) / 1000
	rlng = math.Round(lng*1000) / 1000
	// %.3f, а не сырые float в ключе: -0.0 и 0.0 должны совпадать
	return fmt.Sprintf("%.3f,%.3f", rlat, rlng), rlat, rlng
}

// countryOf — ISO-код страны из набора каналов; «» если страны в наборе нет.
// Ищем по уровню, а не по позиции: набор идёт broad→specific, но планета первая, а
// пустые слоты опускаются — полагаться на индекс нельзя.
func countryOf(channels []Channel) string {
	for _, ch := range channels {
		if ch.Level == "country" {
			return ch.ID
		}
	}
	return ""
}

// record — строка статистики про этот вызов. Долгое ожидание попутно уходит в
// лог: таблица отвечает «сколько раз», лог — «когда и где», а для очереди важно
// именно второе.
func (c *cachedGeocoder) record(source, key string, wait time.Duration, channels []Channel, err error) {
	if wait >= geocodeSlowWait {
		slog.Warn("geocode ждал долго", "cell", key, "wait", wait.Round(time.Millisecond),
			"source", source, "err", err)
	}
	if c.stat == nil {
		return
	}
	c.stat(GeocodeRequest{
		TS:      c.now().UnixMilli(),
		Source:  source,
		Country: countryOf(channels),
		Err:     geocodeErrLabel(err), // «» при успехе
		Wait:    wait,                 // у source=cache в базу уйдёт NULL
	})
}

func (c *cachedGeocoder) Channels(lat, lng float64) ([]Channel, error) {
	key, rlat, rlng := cacheKey(lat, lng)

	c.mu.Lock()
	if e, ok := c.entries[key]; ok && c.now().Sub(e.stored) < c.ttl {
		c.hits++
		out := append([]Channel(nil), e.channels...) // копия: кеш неизменяем для вызывающего
		c.maybeLogStatsLocked()
		c.mu.Unlock()
		c.record(geocodeSourceCache, key, 0, out, nil)
		return out, nil
	}
	// кто-то уже спрашивает эту клетку — ждём его результат
	if call, ok := c.inflight[key]; ok {
		// hits — про экономию СЕТИ, её этот вызов действительно экономит. А вот в
		// статистику ожидания он идёт как запрос (joined), а не как попадание.
		c.hits++
		c.mu.Unlock()
		// Ожидание здесь настоящее (чужой запрос — те же ~2,2 с плюс его очередь),
		// поэтому это НЕ попадание в кеш: время платит и этот вызывающий.
		start := c.now()
		<-call.done
		out := append([]Channel(nil), call.channels...)
		c.record(geocodeSourceJoined, key, c.now().Sub(start), out, call.err)
		if call.err != nil {
			return nil, call.err
		}
		return out, nil
	}
	c.misses++
	call := &geocodeCall{done: make(chan struct{})}
	c.inflight[key] = call
	c.mu.Unlock()

	// Замер вокруг inner: сюда попадает и очередь на мьютексе геокодера, и пауза
	// дросселя 1100 мс, и сама сеть — то есть ровно то, что ждёт человек.
	start := c.now()
	call.channels, call.err = c.inner.Channels(rlat, rlng)
	wait := c.now().Sub(start)

	c.mu.Lock()
	delete(c.inflight, key)
	// Ошибки НЕ кешируем: сетевой сбой или таймаут Nominatim иначе отравил бы
	// клетку на сутки.
	if call.err == nil {
		c.evictLocked()
		c.entries[key] = cacheEntry{channels: call.channels, stored: c.now()}
	}
	c.mu.Unlock()

	close(call.done)
	// При отказе страна пустая не случайно: мы её так и не узнали.
	c.record(geocodeSourceNet, key, wait, call.channels, call.err)
	if call.err != nil {
		return nil, call.err
	}
	return append([]Channel(nil), call.channels...), nil
}

// evictLocked освобождает место, если кеш дорос до предела: сначала просроченные,
// затем самые старые. Вызывать под mu.
func (c *cachedGeocoder) evictLocked() {
	if len(c.entries) < c.max {
		return
	}
	now := c.now()
	for k, e := range c.entries {
		if now.Sub(e.stored) >= c.ttl {
			delete(c.entries, k)
		}
	}
	// всё ещё тесно — выбрасываем десятую часть самых старых
	for len(c.entries) >= c.max {
		var oldestKey string
		var oldest time.Time
		for k, e := range c.entries {
			if oldestKey == "" || e.stored.Before(oldest) {
				oldestKey, oldest = k, e.stored
			}
		}
		delete(c.entries, oldestKey)
	}
}

func (c *cachedGeocoder) maybeLogStatsLocked() {
	if c.now().Sub(c.lastStats) < geocodeStatsEvery {
		return
	}
	c.lastStats = c.now()
	total := c.hits + c.misses
	rate := 0.0
	if total > 0 {
		rate = float64(c.hits) / float64(total) * 100
	}
	slog.Info("geocode cache", "hits", c.hits, "misses", c.misses,
		"hit_rate_pct", int(rate), "size", len(c.entries))
}

// Stats — для тестов и диагностики.
func (c *cachedGeocoder) Stats() (hits, misses int64, size int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits, c.misses, len(c.entries)
}
