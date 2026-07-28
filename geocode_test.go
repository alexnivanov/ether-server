package main

import "testing"

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
