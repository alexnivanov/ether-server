package main

import (
	"testing"
	"time"
)

// old — возраст «обжившегося» аккаунта: на нём работают тиры по репутации, а не
// узкий тир для свежих (см. newAccountWindow).
const old = 24 * time.Hour

// Время в лимитере подменяемое (поле now), поэтому тесты не спят: двигаем часы
// руками и проверяем именно арифметику бакета.
func TestRateLimiterBurstAndRefill(t *testing.T) {
	base := messageLimitFor(0, old) // базовый тир: capacity 5, refill 3с
	now := time.Now()
	r := NewRateLimiter()
	r.now = func() time.Time { return now }

	// всплеск: capacity сообщений подряд проходят
	for i := 0; i < base.capacity; i++ {
		if ok, _ := r.Allow(1, 0, old); !ok {
			t.Fatalf("сообщение %d из всплеска отбито, а должно пройти", i+1)
		}
	}
	// следующее — уже нет, и нам говорят сколько ждать
	ok, retry := r.Allow(1, 0, old)
	if ok {
		t.Fatal("после исчерпания всплеска сообщение прошло")
	}
	if retry <= 0 || retry > base.refill {
		t.Fatalf("retryAfter = %v, ожидали (0, %v]", retry, base.refill)
	}

	// подождали один refill — ровно одно сообщение снова можно
	now = now.Add(base.refill)
	if ok, _ := r.Allow(1, 0, old); !ok {
		t.Fatal("после refill сообщение не прошло")
	}
	if ok, _ := r.Allow(1, 0, old); ok {
		t.Fatal("refill дал больше одного сообщения")
	}

	// долив ограничен capacity: за час не накапливается бесконечный запас
	now = now.Add(time.Hour)
	for i := 0; i < base.capacity; i++ {
		if ok, _ := r.Allow(1, 0, old); !ok {
			t.Fatalf("после долгой паузы сообщение %d отбито", i+1)
		}
	}
	if ok, _ := r.Allow(1, 0, old); ok {
		t.Fatal("бакет перелился выше capacity")
	}

	// лимит на аккаунт, а не глобальный: другой пользователь не задет
	if ok, _ := r.Allow(2, 0, old); !ok {
		t.Fatal("исчерпанный лимит одного пользователя задел другого")
	}
}

// Тиры поощряющие: плюсы поднимают лимит выше базового, минусы возвращают к
// узкому дну, но не затыкают совсем (иначе минусы становятся оружием).
func TestMessageLimitTiers(t *testing.T) {
	low, base, high := messageLimitFor(-3, old), messageLimitFor(0, old), messageLimitFor(10, old)

	if !(low.capacity < base.capacity && base.capacity < high.capacity) {
		t.Fatalf("capacity не растёт с репутацией: %d / %d / %d",
			low.capacity, base.capacity, high.capacity)
	}
	if !(low.refill > base.refill && base.refill > high.refill) {
		t.Fatalf("темп не растёт с репутацией: %v / %v / %v",
			low.refill, base.refill, high.refill)
	}
	if low.capacity < 1 {
		t.Fatal("на дне нельзя писать вообще — минусы не должны затыкать полностью")
	}
	// нулевая репутация — это и есть плоский лимит: пока голосов нет, все здесь
	if base != messageLimitFor(4, old) {
		t.Fatal("база должна покрывать диапазон 0..4")
	}
}

// Свежий аккаунт пишет медленнее любого обжившегося — этим ферма одноразовых
// аккаунтов и обесценивается. Возраст перебивает репутацию: иначе тир накрутили
// бы плюсами с тех же свежих аккаунтов.
func TestMessageLimitNewAccount(t *testing.T) {
	fresh := messageLimitFor(0, time.Minute)
	base := messageLimitFor(0, old)
	if fresh.capacity >= base.capacity || fresh.refill <= base.refill {
		t.Fatalf("свежий аккаунт не ограничен: %+v vs база %+v", fresh, base)
	}
	if fresh.capacity < 1 {
		t.Fatal("новичку нельзя написать вообще — это уже не антиспам, а стена")
	}
	// высокая репутация не отменяет тир свежести
	if messageLimitFor(10, time.Minute) != fresh {
		t.Fatal("репутация перебила возраст аккаунта")
	}
	// после окна — обычный тир
	if messageLimitFor(0, newAccountWindow+time.Second) != base {
		t.Fatal("после newAccountWindow аккаунт не получил базовый тир")
	}
}

// Смена тира между сообщениями не должна давать запас больше нового capacity.
func TestRateLimiterCapsOnTierChange(t *testing.T) {
	now := time.Now()
	r := NewRateLimiter()
	r.now = func() time.Time { return now }

	// накопили запас как высокорейтинговый
	if ok, _ := r.Allow(7, 10, old); !ok {
		t.Fatal("первое сообщение отбито")
	}
	now = now.Add(time.Hour) // бакет полон по высокому тиру

	// репутация упала — доступно не больше нового (узкого) capacity
	low := messageLimitFor(-1, old)
	for i := 0; i < low.capacity; i++ {
		if ok, _ := r.Allow(7, -1, old); !ok {
			t.Fatalf("сообщение %d на низком тире отбито", i+1)
		}
	}
	if ok, _ := r.Allow(7, -1, old); ok {
		t.Fatal("после падения репутации остался запас высокого тира")
	}
}
