package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Планета есть всегда и идёт первой: она не выводится из координат, а
// добавляется константой (см. PlanetChannel). Проверяем на StubGeocoder —
// у реального геокодера то же место в наборе, но он ходит в сеть (см.
// nominatim_test.go под тегом live).
func TestPlanetChannelAlwaysFirst(t *testing.T) {
	chans, err := StubGeocoder{}.Channels(55.76, 37.61)
	if err != nil {
		t.Fatalf("channels: %v", err)
	}
	if len(chans) == 0 {
		t.Fatal("пустой набор каналов")
	}
	if chans[0] != PlanetChannel {
		t.Fatalf("первый канал = %+v, want %+v (набор идёт broad→specific)", chans[0], PlanetChannel)
	}
	if PlanetChannel.Level != "planet" || PlanetChannel.ID != "EARTH" {
		t.Fatalf("контракт планеты изменился: %+v", PlanetChannel)
	}
	// ID планеты не должен выглядеть как код ISO 3166-1 (те строго двухбуквенные),
	// иначе однажды столкнётся с настоящей страной
	if len(PlanetChannel.ID) == 2 {
		t.Fatalf("ID %q двухбуквенный — риск коллизии с ISO 3166-1", PlanetChannel.ID)
	}
}

// TestChannelsWaterGivesPlanetOnly — в точке без единой административной
// единицы (открытая вода, полюс) Nominatim отвечает 200 с полем error, и это
// не отказ: набор из одной Планеты честнее ошибки. Пока тут была ошибка,
// клиент не мог войти вовсе — повторял locate, пересобирал соединение и
// показывал «Переподключение…» на живой связи.
func TestChannelsWaterGivesPlanetOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// ответ настоящего Nominatim на точку посреди океана
		w.Write([]byte(`{"error":"Unable to geocode"}`))
	}))
	defer srv.Close()

	g := NewNominatimGeocoder()
	g.BaseURL = srv.URL
	g.minInterval = 0 // дроссель публичного сервера тут ни к чему

	chans, err := g.Channels(0, -40) // Атлантика
	if err != nil {
		t.Fatalf("channels: %v — вода не отказ", err)
	}
	if len(chans) != 1 || chans[0] != PlanetChannel {
		t.Fatalf("набор = %+v, want только %+v", chans, PlanetChannel)
	}
}
