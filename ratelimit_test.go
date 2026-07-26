package main

import (
	"testing"
	"time"
)

// Время в лимитере подменяемое (поле now), поэтому тесты не спят: двигаем часы
// руками и проверяем именно арифметику бакета.
func TestRateLimiterBurstAndRefill(t *testing.T) {
	base := messageLimitFor(0) // базовый тир: capacity 5, refill 3с
	now := time.Now()
	r := NewRateLimiter()
	r.now = func() time.Time { return now }

	// всплеск: capacity сообщений подряд проходят
	for i := 0; i < base.capacity; i++ {
		if ok, _ := r.Allow(1, 0); !ok {
			t.Fatalf("сообщение %d из всплеска отбито, а должно пройти", i+1)
		}
	}
	// следующее — уже нет, и нам говорят сколько ждать
	ok, retry := r.Allow(1, 0)
	if ok {
		t.Fatal("после исчерпания всплеска сообщение прошло")
	}
	if retry <= 0 || retry > base.refill {
		t.Fatalf("retryAfter = %v, ожидали (0, %v]", retry, base.refill)
	}

	// подождали один refill — ровно одно сообщение снова можно
	now = now.Add(base.refill)
	if ok, _ := r.Allow(1, 0); !ok {
		t.Fatal("после refill сообщение не прошло")
	}
	if ok, _ := r.Allow(1, 0); ok {
		t.Fatal("refill дал больше одного сообщения")
	}

	// долив ограничен capacity: за час не накапливается бесконечный запас
	now = now.Add(time.Hour)
	for i := 0; i < base.capacity; i++ {
		if ok, _ := r.Allow(1, 0); !ok {
			t.Fatalf("после долгой паузы сообщение %d отбито", i+1)
		}
	}
	if ok, _ := r.Allow(1, 0); ok {
		t.Fatal("бакет перелился выше capacity")
	}

	// лимит на аккаунт, а не глобальный: другой пользователь не задет
	if ok, _ := r.Allow(2, 0); !ok {
		t.Fatal("исчерпанный лимит одного пользователя задел другого")
	}
}

// Тиры поощряющие: плюсы поднимают лимит выше базового, минусы возвращают к
// узкому дну, но не затыкают совсем (иначе минусы становятся оружием).
func TestMessageLimitTiers(t *testing.T) {
	low, base, high := messageLimitFor(-3), messageLimitFor(0), messageLimitFor(10)

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
	if base != messageLimitFor(4) {
		t.Fatal("база должна покрывать диапазон 0..4")
	}
}

// Смена тира между сообщениями не должна давать запас больше нового capacity.
func TestRateLimiterCapsOnTierChange(t *testing.T) {
	now := time.Now()
	r := NewRateLimiter()
	r.now = func() time.Time { return now }

	// накопили запас как высокорейтинговый
	if ok, _ := r.Allow(7, 10); !ok {
		t.Fatal("первое сообщение отбито")
	}
	now = now.Add(time.Hour) // бакет полон по высокому тиру

	// репутация упала — доступно не больше нового (узкого) capacity
	low := messageLimitFor(-1)
	for i := 0; i < low.capacity; i++ {
		if ok, _ := r.Allow(7, -1); !ok {
			t.Fatalf("сообщение %d на низком тире отбито", i+1)
		}
	}
	if ok, _ := r.Allow(7, -1); ok {
		t.Fatal("после падения репутации остался запас высокого тира")
	}
}
