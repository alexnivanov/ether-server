package main

import (
	"testing"
	"time"
)

// old — возраст «обжившегося» аккаунта: на нём работают тиры по репутации, а не
// узкий тир для свежих (см. newAccountWindow).
const old = 24 * time.Hour

// localChannel — канал, у которого своего тира нет и не планируется: район
// (`osm_type/osm_id`). Тесты про тиры по репутации и возрасту ходят именно сюда,
// иначе они незаметно проверяют тир Земли или страны — у тира свежего аккаунта и
// у страны совпадает capacity, так что подмена выглядела бы как зелёный тест.
const localChannel = "relation/1320555"

// Время в лимитере подменяемое (поле now), поэтому тесты не спят: двигаем часы
// руками и проверяем именно арифметику бакета.
func TestRateLimiterBurstAndRefill(t *testing.T) {
	base := messageLimitFor(0, old, scopeLocal) // базовый тир: capacity 5, refill 3с
	now := time.Now()
	r := NewRateLimiter()
	r.now = func() time.Time { return now }

	// всплеск: capacity сообщений подряд проходят
	for i := 0; i < base.capacity; i++ {
		if ok, _ := r.Allow(1, 0, old, scopeLocal); !ok {
			t.Fatalf("сообщение %d из всплеска отбито, а должно пройти", i+1)
		}
	}
	// следующее — уже нет, и нам говорят сколько ждать
	ok, retry := r.Allow(1, 0, old, scopeLocal)
	if ok {
		t.Fatal("после исчерпания всплеска сообщение прошло")
	}
	if retry <= 0 || retry > base.refill {
		t.Fatalf("retryAfter = %v, ожидали (0, %v]", retry, base.refill)
	}

	// подождали один refill — ровно одно сообщение снова можно
	now = now.Add(base.refill)
	if ok, _ := r.Allow(1, 0, old, scopeLocal); !ok {
		t.Fatal("после refill сообщение не прошло")
	}
	if ok, _ := r.Allow(1, 0, old, scopeLocal); ok {
		t.Fatal("refill дал больше одного сообщения")
	}

	// долив ограничен capacity: за час не накапливается бесконечный запас
	now = now.Add(time.Hour)
	for i := 0; i < base.capacity; i++ {
		if ok, _ := r.Allow(1, 0, old, scopeLocal); !ok {
			t.Fatalf("после долгой паузы сообщение %d отбито", i+1)
		}
	}
	if ok, _ := r.Allow(1, 0, old, scopeLocal); ok {
		t.Fatal("бакет перелился выше capacity")
	}

	// лимит на аккаунт, а не глобальный: другой пользователь не задет
	if ok, _ := r.Allow(2, 0, old, scopeLocal); !ok {
		t.Fatal("исчерпанный лимит одного пользователя задел другого")
	}
}

// Внизу лестницы минус тормозит, а плюс НЕ разгоняет: разницу между 20/мин и
// 40/мин живой человек не выбирает даже намеренно, зато это ровно та награда,
// ради которой имеет смысл накручивать. Плюс покупает темп только в широких
// полосах (см. TestMessageLimitPlanetTier).
func TestMessageLimitTiers(t *testing.T) {
	low := messageLimitFor(-3, old, scopeLocal)
	base := messageLimitFor(0, old, scopeLocal)
	high := messageLimitFor(10, old, scopeLocal)

	if !(low.capacity < base.capacity && low.refill > base.refill) {
		t.Fatalf("минус не тормозит: дно %+v, база %+v", low, base)
	}
	if low.capacity < 1 {
		t.Fatal("на дне нельзя писать вообще — минусы не должны затыкать полностью")
	}
	if high != base {
		t.Fatalf("плюс разгоняет локальный тир: %+v вместо базы %+v", high, base)
	}
	// нулевая репутация — это и есть плоский лимит
	if base != messageLimitFor(4, old, scopeLocal) {
		t.Fatal("база должна покрывать диапазон 0..4")
	}
}

// Свежий аккаунт пишет медленнее любого обжившегося — этим ферма одноразовых
// аккаунтов и обесценивается. Возраст перебивает репутацию: иначе тир накрутили
// бы плюсами с тех же свежих аккаунтов.
func TestMessageLimitNewAccount(t *testing.T) {
	fresh := messageLimitFor(0, time.Minute, scopeLocal)
	base := messageLimitFor(0, old, scopeLocal)
	if fresh.capacity >= base.capacity || fresh.refill <= base.refill {
		t.Fatalf("свежий аккаунт не ограничен: %+v vs база %+v", fresh, base)
	}
	if fresh.capacity < 1 {
		t.Fatal("новичку нельзя написать вообще — это уже не антиспам, а стена")
	}
	// высокая репутация не отменяет тир свежести
	if messageLimitFor(10, time.Minute, scopeLocal) != fresh {
		t.Fatal("репутация перебила возраст аккаунта")
	}
	// после окна — обычный тир
	if messageLimitFor(0, newAccountWindow+time.Second, scopeLocal) != base {
		t.Fatal("после newAccountWindow аккаунт не получил базовый тир")
	}
}

// Смена тира между сообщениями не должна давать запас больше нового capacity.
func TestRateLimiterCapsOnTierChange(t *testing.T) {
	now := time.Now()
	r := NewRateLimiter()
	r.now = func() time.Time { return now }

	// накопили запас как высокорейтинговый
	if ok, _ := r.Allow(7, 10, old, scopeLocal); !ok {
		t.Fatal("первое сообщение отбито")
	}
	now = now.Add(time.Hour) // бакет полон по высокому тиру

	// репутация упала — доступно не больше нового (узкого) capacity
	low := messageLimitFor(-1, old, scopeLocal)
	for i := 0; i < low.capacity; i++ {
		if ok, _ := r.Allow(7, -1, old, scopeLocal); !ok {
			t.Fatalf("сообщение %d на низком тире отбито", i+1)
		}
	}
	if ok, _ := r.Allow(7, -1, old, scopeLocal); ok {
		t.Fatal("после падения репутации остался запас высокого тира")
	}
}

// Ожидание округляется ВВЕРХ до минуты и меняет единицу: «подожди 3421 с» на
// часовом лимите не читается, «через 2 ч» при паузе 1 ч 30 мин врёт на полчаса,
// а «через 45 мин» при остатке 45 мин 30 с вернуло бы человека к тому же отказу.
func TestHumanWait(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{2 * time.Second, "3 с"},
		{59*time.Second + 900*time.Millisecond, "60 с"},
		{time.Minute, "1 мин"},
		{45*time.Minute + 30*time.Second, "46 мин"},
		{59*time.Minute + 30*time.Second, "1 ч"},
		{time.Hour, "1 ч"},
		{time.Hour + 30*time.Second, "1 ч 1 мин"},
		{2*time.Hour + 29*time.Minute + 10*time.Second, "2 ч 30 мин"},
		{24 * time.Hour, "24 ч"},
	}
	for _, c := range cases {
		if got := humanWait(c.d); got != c.want {
			t.Errorf("humanWait(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

// Земля стоит дороже любого локального канала: одно сообщение в час на нулевой
// репутации. Возраст аккаунта здесь не при чём (1/час строже любого локального
// тира), а рейтинг только тормозит, см. TestMessageLimitPlanetRating.
func TestMessageLimitPlanetTier(t *testing.T) {
	planet := messageLimitFor(0, old, scopePlanet)
	if planet.capacity != 1 || planet.refill != time.Hour {
		t.Fatalf("Земля не 1/час: %+v", planet)
	}
	for _, tier := range []messageLimit{
		messageLimitFor(0, old, scopeLocal),
		messageLimitFor(100, old, scopeLocal),
		messageLimitFor(0, time.Minute, scopeLocal), // свежий аккаунт — самый узкий локальный
	} {
		if planet.refill <= tier.refill || planet.capacity > tier.capacity {
			t.Fatalf("Земля не строже локального тира %+v: %+v", tier, planet)
		}
	}
	if messageLimitFor(0, time.Minute, scopePlanet) != planet {
		t.Fatal("возраст аккаунта изменил тир Земли")
	}
}

// Рейтинг в Земле: каждый минус добавляет полчаса к часовой паузе, не дальше
// суток, каждый плюс делит паузу пополам, не быстрее раза в минуту.
func TestMessageLimitPlanetRating(t *testing.T) {
	cases := []struct {
		rating int
		want   time.Duration
	}{
		{1 << 40, time.Minute},
		{7, time.Minute},
		{6, time.Minute}, // 56 с — уже под полом
		{5, 112500 * time.Millisecond},
		{3, 7*time.Minute + 30*time.Second},
		{1, 30 * time.Minute},
		{0, time.Hour},
		{-1, time.Hour + 30*time.Minute},
		{-3, 2*time.Hour + 30*time.Minute},
		{-46, 24 * time.Hour},
		{-47, 24 * time.Hour},
		{-1 << 40, 24 * time.Hour}, // без переполнения Duration
	}
	for _, c := range cases {
		got := messageLimitFor(c.rating, old, scopePlanet)
		if got.capacity != 1 || got.refill != c.want {
			t.Errorf("рейтинг %d: %+v, want 1 / %v", c.rating, got, c.want)
		}
	}
}

// Область и город — своя полоса между страной и локальным: мягче страны, строже
// района, а минус тормозит и здесь.
func TestMessageLimitCityTier(t *testing.T) {
	city := messageLimitFor(0, old, scopeCity)
	if city != cityLimit {
		t.Fatalf("тир города = %+v, want %+v", city, cityLimit)
	}
	country := messageLimitFor(0, old, scopeCountry)
	if city.refill >= country.refill {
		t.Fatalf("город не мягче страны: %+v против %+v", city, country)
	}
	local := messageLimitFor(0, old, scopeLocal)
	if city.refill <= local.refill {
		t.Fatalf("город не строже локального: %+v против %+v", city, local)
	}
	if bad := messageLimitFor(-1, old, scopeCity); bad.refill <= city.refill {
		t.Fatalf("минус не тормозит город: %+v против %+v", bad, city)
	}
}

// Тир страны лежит между Землёй и локальным: строже любого локального (в том
// числе узкого тира свежих аккаунтов), но мягче часовой Земли.
func TestMessageLimitCountryTier(t *testing.T) {
	country := messageLimitFor(0, old, scopeCountry)
	if country.capacity != 2 || country.refill != 5*time.Minute {
		t.Fatalf("тир страны = %+v, want 2 / 5 мин", country)
	}
	planet := messageLimitFor(0, old, scopePlanet)
	if country.refill >= planet.refill {
		t.Fatalf("страна не мягче Земли: %+v vs %+v", country, planet)
	}
	for _, tier := range []messageLimit{
		messageLimitFor(0, old, scopeLocal),
		messageLimitFor(100, old, scopeLocal),
		messageLimitFor(0, time.Minute, scopeLocal), // свежий аккаунт
	} {
		if country.refill <= tier.refill || country.capacity > tier.capacity {
			t.Fatalf("страна не строже локального тира %+v: %+v", tier, country)
		}
	}
	if messageLimitFor(100, time.Hour*10000, scopeCountry) != country {
		t.Fatal("плюсы или возраст изменили тир страны")
	}
}

// Минус в стране: +2,5 мин к паузе за каждый, не дальше часа, и без всплеска.
func TestMessageLimitCountryRating(t *testing.T) {
	cases := []struct {
		rating int
		want   time.Duration
	}{
		{-1, 7*time.Minute + 30*time.Second},
		{-3, 12*time.Minute + 30*time.Second},
		{-22, time.Hour},
		{-23, time.Hour},
		{-1 << 40, time.Hour},
	}
	for _, c := range cases {
		got := messageLimitFor(c.rating, old, scopeCountry)
		if got.capacity != 1 || got.refill != c.want {
			t.Errorf("рейтинг %d: %+v, want 1 / %v", c.rating, got, c.want)
		}
	}
}

// Разгон в Земле считается только по голосам в Земле: плюсы из района, где
// писать можно 20 раз в минуту, его не дают. Общий минус перебивает плюсы в
// Земле.
func TestPlanetRating(t *testing.T) {
	cases := []struct {
		total, planet, want int
	}{
		{0, 0, 0},
		{10, 0, 0},   // плюсы только из других каналов — разгона нет
		{10, 3, 3},   // разгон по Земле
		{3, 5, 5},    // в других каналах минусы, но в сумме ≥ 0
		{10, -2, 0},  // в Земле минусы — не тормозят сверх общего, но и не разгоняют
		{-2, 5, -2},  // общий минус перебивает плюсы в Земле
		{-4, -4, -4}, // минусы — из общего рейтинга
	}
	for _, c := range cases {
		if got := planetRating(c.total, c.planet); got != c.want {
			t.Errorf("planetRating(%d, %d) = %d, want %d", c.total, c.planet, got, c.want)
		}
	}
}

// У страны свой бакет: он не общий ни с Землёй, ни с локальными каналами.
func TestCountryBucketSeparate(t *testing.T) {
	now := time.Now()
	r := NewRateLimiter()
	r.now = func() time.Time { return now }

	for i := 0; i < countryLimit(0).capacity; i++ {
		if ok, _ := r.Allow(1, 0, old, scopeCountry); !ok {
			t.Fatalf("сообщение в страну %d отбито", i+1)
		}
	}
	if ok, _ := r.Allow(1, 0, old, scopeCountry); ok {
		t.Fatal("всплеск страны не кончился")
	}
	if ok, _ := r.Allow(1, 0, old, scopeLocal); !ok {
		t.Fatal("исчерпанная страна закрыла локальный канал")
	}
	if ok, _ := r.Allow(1, 0, old, scopePlanet); !ok {
		t.Fatal("исчерпанная страна закрыла Землю")
	}
}

// Запасы раздельные: сообщение в Землю не съедает локальный темп и наоборот.
// Иначе одна реплика на весь мир на час затыкала бы человека в его квартале.
func TestPlanetBucketSeparateFromLocal(t *testing.T) {
	now := time.Now()
	r := NewRateLimiter()
	r.now = func() time.Time { return now }

	// исчерпали локальный всплеск целиком
	base := messageLimitFor(0, old, scopeLocal)
	for i := 0; i < base.capacity; i++ {
		if ok, _ := r.Allow(1, 0, old, scopeLocal); !ok {
			t.Fatalf("локальное сообщение %d отбито", i+1)
		}
	}
	if ok, _ := r.Allow(1, 0, old, scopeLocal); ok {
		t.Fatal("локальный лимит не сработал")
	}
	// Земля при этом доступна: у неё свой запас
	if ok, _ := r.Allow(1, 0, old, scopePlanet); !ok {
		t.Fatal("исчерпанный локальный лимит закрыл Землю")
	}
	// и в обратную сторону: Земля исчерпана, локальный темп восстановился
	ok, retry := r.Allow(1, 0, old, scopePlanet)
	if ok {
		t.Fatal("второе сообщение в Землю прошло сразу")
	}
	if retry <= 55*time.Minute || retry > time.Hour {
		t.Fatalf("retryAfter = %v, ожидали около часа", retry)
	}
	now = now.Add(base.refill)
	if ok, _ := r.Allow(1, 0, old, scopeLocal); !ok {
		t.Fatal("исчерпанная Земля закрыла локальный канал")
	}
}

// Главная грабля часового лимита: уборка бакетов не должна превращать «раз в
// час» в «раз в столько, через сколько бакет забывают». Пустой бакет удалить
// нельзя — новый создаётся ПОЛНЫМ, и следующее сообщение прошло бы.
func TestPlanetBucketSurvivesSweep(t *testing.T) {
	now := time.Now()
	r := NewRateLimiter()
	r.now = func() time.Time { return now }

	const flooder = 1
	if ok, _ := r.Allow(flooder, 0, old, scopePlanet); !ok {
		t.Fatal("первое сообщение в Землю отбито")
	}
	// Уборка включается только на заполненной map (порог 64), поэтому набиваем
	// её чужими бакетами — иначе тест проверял бы не то, что нужно.
	for id := int64(2); id <= 200; id++ {
		r.Allow(id, 0, old, scopeLocal)
	}
	if len(r.buckets) < 64 {
		t.Fatalf("бакетов %d — порог уборки не пройден, тест бессмыслен", len(r.buckets))
	}

	// молчим дольше прежнего порога простоя (он был 10 минут) и заведомо меньше часа
	now = now.Add(15 * time.Minute)
	r.Allow(500, 0, old, scopeLocal) // чей-то запрос — в нём и происходит уборка
	if ok, retry := r.Allow(flooder, 0, old, scopePlanet); ok {
		t.Fatalf("через 15 минут в Землю прошло второе сообщение (retry=%v)", retry)
	}

	// а через час — можно
	now = now.Add(45 * time.Minute)
	if ok, _ := r.Allow(flooder, 0, old, scopePlanet); !ok {
		t.Fatal("через час сообщение в Землю не прошло")
	}
}

// Полный бакет из map выбрасывается: он ничем не отличается от нового, а без
// уборки map росла бы вместе с числом когда-либо писавших.
func TestSweepDropsFullBuckets(t *testing.T) {
	now := time.Now()
	r := NewRateLimiter()
	r.now = func() time.Time { return now }

	for id := int64(1); id <= 200; id++ {
		r.Allow(id, 0, old, scopeLocal)
	}
	before := len(r.buckets)
	// все успели долиться до capacity
	now = now.Add(time.Hour)
	r.Allow(1, 0, old, scopeLocal)
	if len(r.buckets) >= before {
		t.Fatalf("бакетов было %d, стало %d — полные не выброшены", before, len(r.buckets))
	}
}

// TestIPLimiterBurstRefillAndKeys — бакет по адресу: всплеск, доливка и главное
// — разные адреса не делят запас (иначе один любопытный закрыл бы лендинг всем).
func TestIPLimiterBurstAndKeys(t *testing.T) {
	lim := messageLimit{capacity: 2, refill: 15 * time.Second}
	now := time.Now()
	l := newIPLimiter(lim)
	l.now = func() time.Time { return now }

	for i := 0; i < lim.capacity; i++ {
		if ok, _ := l.Allow("203.0.113.7"); !ok {
			t.Fatalf("запрос %d из всплеска отбит, а должен пройти", i+1)
		}
	}
	ok, retry := l.Allow("203.0.113.7")
	if ok {
		t.Fatal("после исчерпания всплеска запрос прошёл")
	}
	if retry <= 0 || retry > lim.refill {
		t.Fatalf("retryAfter = %v, ожидали (0, %v]", retry, lim.refill)
	}

	// сосед по улице, но не по бакету
	if ok, _ := l.Allow("203.0.113.8"); !ok {
		t.Fatal("другой адрес отбит чужим лимитом")
	}

	now = now.Add(lim.refill)
	if ok, _ := l.Allow("203.0.113.7"); !ok {
		t.Fatal("после refill запрос не прошёл")
	}
	if ok, _ := l.Allow("203.0.113.7"); ok {
		t.Fatal("refill долил больше одного запроса")
	}
}
