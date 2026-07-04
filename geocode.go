package main

// Channel — одна административная единица, в чате которой состоит пользователь.
// Формат ID — межпроектный контракт (ether-meta/CLAUDE.md): ISO 3166-1/-2 для
// страны/области, osm_type/osm_id для города/района/квартала.
type Channel struct {
	ID    string `json:"id"`    // стабильный ключ канала: "RU", "RU-MOW", "relation/2555133"
	Level string `json:"level"` // country | region | city | district | quarter
	Label string `json:"label"` // подпись уровня для UI: "Город"
	Name  string `json:"name"`  // отображаемое имя: "Москва"
}

// Geocoder: координаты → упорядоченный набор каналов (broad→specific).
// Пустые слоты опускаются. Реализация ОБЯЗАНА быть детерминированной: одни и те
// же координаты → один и тот же набор ID (от этого зависит корректность чата).
type Geocoder interface {
	Channels(lat, lng float64) ([]Channel, error)
}

// StubGeocoder возвращает фиксированный набор каналов независимо от входа.
// Позволяет прогнать чат сквозняком (подписка/рассылка) ещё до реального
// геокодера и без обращения в сеть.
type StubGeocoder struct{}

func (StubGeocoder) Channels(lat, lng float64) ([]Channel, error) {
	return []Channel{
		{ID: "RU", Level: "country", Label: "Страна", Name: "Россия"},
		{ID: "RU-MOW", Level: "region", Label: "Область", Name: "Москва"},
		{ID: "relation/2555133", Level: "city", Label: "Город", Name: "Москва"},
		{ID: "relation/1320555", Level: "district", Label: "Район", Name: "Тверской"},
	}, nil
}
