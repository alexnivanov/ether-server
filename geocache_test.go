package main

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// countingGeocoder считает обращения и запоминает, с какими координатами его
// позвали — нужно проверить, что кеш геокодит клетку, а не точку вызывающего.
type countingGeocoder struct {
	mu    sync.Mutex
	calls int
	args  [][2]float64
	delay time.Duration
	err   error
}

func (g *countingGeocoder) Channels(lat, lng float64) ([]Channel, error) {
	g.mu.Lock()
	g.calls++
	g.args = append(g.args, [2]float64{lat, lng})
	err, delay := g.err, g.delay
	g.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
	if err != nil {
		return nil, err
	}
	return []Channel{PlanetChannel, {ID: "RU", Level: "country", Name: "Россия"}}, nil
}

func (g *countingGeocoder) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

func TestGeocodeCacheRoundsToCell(t *testing.T) {
	inner := &countingGeocoder{}
	c := newCachedGeocoder(inner, time.Hour, 100)

	// три точки в пределах одной клетки 0.001° (~100 м): 55.7558 / 55.75561 /
	// 55.75604 округляются к 55.756
	for _, p := range [][2]float64{{55.7558, 37.6173}, {55.75561, 37.61729}, {55.75604, 37.61734}} {
		if _, err := c.Channels(p[0], p[1]); err != nil {
			t.Fatalf("channels%v: %v", p, err)
		}
	}
	if inner.count() != 1 {
		t.Fatalf("геокодер вызван %d раз, want 1 (одна клетка)", inner.count())
	}
	// геокодили именно округлённые координаты, а не координаты первого вызова
	if inner.args[0] != [2]float64{55.756, 37.617} {
		t.Fatalf("геокодер позван с %v, want округлённые (55.756, 37.617)", inner.args[0])
	}

	// соседняя клетка — отдельный запрос
	if _, err := c.Channels(55.757, 37.617); err != nil {
		t.Fatal(err)
	}
	if inner.count() != 2 {
		t.Fatalf("для соседней клетки вызовов %d, want 2", inner.count())
	}

	hits, misses, size := c.Stats()
	if hits != 2 || misses != 2 || size != 2 {
		t.Fatalf("stats: hits=%d misses=%d size=%d, want 2/2/2", hits, misses, size)
	}
}

// Одновременные запросы к одной клетке должны склеиваться в один геокодинг:
// иначе десять человек в одном дворе выстроятся в очередь на десять запросов по
// 2.2 с каждый.
func TestGeocodeCacheSingleFlight(t *testing.T) {
	inner := &countingGeocoder{delay: 80 * time.Millisecond}
	c := newCachedGeocoder(inner, time.Hour, 100)

	var wg sync.WaitGroup
	errs := make(chan error, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// все точки в одной клетке
			_, err := c.Channels(55.7560+float64(i)*0.00001, 37.6170)
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("параллельный запрос: %v", err)
		}
	}
	if inner.count() != 1 {
		t.Fatalf("геокодер вызван %d раз, want 1 (single-flight)", inner.count())
	}
}

// Ошибки не кешируются: иначе сетевой сбой отравил бы клетку на сутки.
func TestGeocodeCacheDoesNotCacheErrors(t *testing.T) {
	inner := &countingGeocoder{err: errors.New("nominatim упал")}
	c := newCachedGeocoder(inner, time.Hour, 100)

	if _, err := c.Channels(55.756, 37.617); err == nil {
		t.Fatal("ожидали ошибку")
	}
	inner.mu.Lock()
	inner.err = nil
	inner.mu.Unlock()

	if _, err := c.Channels(55.756, 37.617); err != nil {
		t.Fatalf("повторный запрос после сбоя: %v", err)
	}
	if inner.count() != 2 {
		t.Fatalf("вызовов %d, want 2 (ошибка не должна кешироваться)", inner.count())
	}
}

func TestGeocodeCacheTTLAndEviction(t *testing.T) {
	inner := &countingGeocoder{}
	now := time.Now()
	c := newCachedGeocoder(inner, time.Hour, 100)
	c.now = func() time.Time { return now }

	if _, err := c.Channels(55.756, 37.617); err != nil {
		t.Fatal(err)
	}
	now = now.Add(30 * time.Minute) // ещё свежо
	if _, err := c.Channels(55.756, 37.617); err != nil {
		t.Fatal(err)
	}
	if inner.count() != 1 {
		t.Fatalf("внутри TTL вызовов %d, want 1", inner.count())
	}
	now = now.Add(2 * time.Hour) // TTL истёк
	if _, err := c.Channels(55.756, 37.617); err != nil {
		t.Fatal(err)
	}
	if inner.count() != 2 {
		t.Fatalf("после TTL вызовов %d, want 2", inner.count())
	}

	// потолок записей соблюдается
	small := newCachedGeocoder(&countingGeocoder{}, time.Hour, 10)
	for i := 0; i < 50; i++ {
		if _, err := small.Channels(50.0+float64(i)*0.001, 30.0); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, size := small.Stats(); size > 10 {
		t.Fatalf("в кеше %d записей при потолке 10", size)
	}
}

// Кеш не должен отдавать наружу свой слайс: правка результата вызывающим не
// имеет права испортить содержимое кеша.
func TestGeocodeCacheReturnsCopy(t *testing.T) {
	c := newCachedGeocoder(&countingGeocoder{}, time.Hour, 10)
	first, err := c.Channels(55.756, 37.617)
	if err != nil {
		t.Fatal(err)
	}
	first[0] = Channel{ID: "ИСПОРЧЕНО"}
	second, err := c.Channels(55.756, 37.617)
	if err != nil {
		t.Fatal(err)
	}
	if second[0] != PlanetChannel {
		t.Fatalf("кеш испорчен вызывающим: %+v", second[0])
	}
	_ = fmt.Sprint(second)
}
