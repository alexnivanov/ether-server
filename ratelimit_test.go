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

// Ожидание округляется ВВЕРХ и меняет единицу: «подожди 3421 с» на часовом
// лимите не читается, а «через 45 мин» при остатке 45 мин 30 с вернуло бы
// человека к тому же отказу.
func TestHumanWait(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{2 * time.Second, "3 с"},
		{59*time.Second + 900*time.Millisecond, "60 с"},
		{time.Minute, "1 мин"},
		{45*time.Minute + 30*time.Second, "46 мин"},
		{time.Hour, "1 ч"},
	}
	for _, c := range cases {
		if got := humanWait(c.d); got != c.want {
			t.Errorf("humanWait(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

// Земля стоит дороже любого локального канала: одно сообщение в час на нулевой
// репутации. Возраст аккаунта здесь не при чём (1/час строже любого локального
// тира), а рейтинг двигает темп — но в узких пределах, см.
// TestMessageLimitPlanetRating.
func TestMessageLimitPlanetTier(t *testing.T) {
	planet := messageLimitFor(0, old, scopePlanet)
	if planet != planetLimit {
		t.Fatalf("тир Земли = %+v, want %+v", planet, planetLimit)
	}
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
	if messageLimitFor(0, time.Minute, scopePlanet) != planetLimit {
		t.Fatal("возраст аккаунта изменил тир Земли")
	}
}

// Рейтинг в Земле двигает темп, но не открывает дверь: плюсы разгоняют до трёх
// сообщений в час, минусы тормозят вдвое против базы. Потолок разгона здесь
// несущий — при общем запасе голосов второй аккаунт может слить весь запас на
// подельника, и невыгодной такую ферму делает именно мелкость приза.
func TestMessageLimitPlanetRating(t *testing.T) {
	base := messageLimitFor(0, old, scopePlanet)
	good := messageLimitFor(goodRating, old, scopePlanet)
	bad := messageLimitFor(-1, old, scopePlanet)

	if good.refill >= base.refill {
		t.Fatalf("плюс не разгоняет Землю: %+v против базы %+v", good, base)
	}
	if bad.refill <= base.refill {
		t.Fatalf("минус не тормозит Землю: %+v против базы %+v", bad, base)
	}
	// разгон ограничен: даже огромный рейтинг не выводит Землю на локальный темп
	if top := messageLimitFor(1000, old, scopePlanet); top != good {
		t.Fatalf("разгон Земли не упёрся в потолок: %+v", top)
	}
	if good.refill < 15*time.Minute {
		t.Fatalf("Земля разогналась слишком сильно: %+v", good)
	}
}

// Область и город — своя полоса между страной и локальным: устойчивый темп как у
// страны, всплеск меньше, а минус тормозит и здесь.
func TestMessageLimitCityTier(t *testing.T) {
	city := messageLimitFor(0, old, scopeCity)
	if city != cityLimit {
		t.Fatalf("тир города = %+v, want %+v", city, cityLimit)
	}
	country := messageLimitFor(0, old, scopeCountry)
	if city.capacity >= country.capacity {
		t.Fatalf("всплеск города не меньше страны: %+v против %+v", city, country)
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
// числе узкого тира свежих аккаунтов), но мягче часовой Земли — канал страны
// сейчас самая населённая комната, и разговор в нём должен оставаться возможным.
func TestMessageLimitCountryTier(t *testing.T) {
	country := messageLimitFor(0, old, scopeCountry)
	if country != countryLimit {
		t.Fatalf("тир страны = %+v, want %+v", country, countryLimit)
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
	if messageLimitFor(100, time.Hour*10000, scopeCountry) != countryLimit {
		t.Fatal("репутация или возраст изменили тир страны")
	}
}

// У страны свой бакет: он не общий ни с Землёй, ни с локальными каналами.
func TestCountryBucketSeparate(t *testing.T) {
	now := time.Now()
	r := NewRateLimiter()
	r.now = func() time.Time { return now }

	for i := 0; i < countryLimit.capacity; i++ {
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
