package main

import (
	"strings"
	"testing"
	"time"
)

// TestCountryName — подстановка названия и поведение на незнакомом коде: сводка
// не должна терять строку из-за кода, которого нет в словаре.
func TestCountryName(t *testing.T) {
	tests := []struct{ iso, want string }{
		{"RU", "Россия"},
		{"CY", "Кипр"},
		{"NE", "Нигер"},
		{"KZ", "Казахстан"},
		{"ru", "Россия"}, // регистр не наш, а того, кто позвал
		{"ZZ", "ZZ"},     // кода нет в ISO — отдаём как есть
		{"", ""},         // страна не определилась; в сводку такие не попадают
	}
	for _, tt := range tests {
		if got := countryName(tt.iso); got != tt.want {
			t.Errorf("countryName(%q) = %q, want %q", tt.iso, got, tt.want)
		}
	}
}

// TestCountryNamesComplete — словарь полный и опрятный: коды из двух заглавных
// букв, названия непустые. Ловит опечатку в ключе («RUS», «ru»), из-за которой
// страна молча печаталась бы кодом.
func TestCountryNamesComplete(t *testing.T) {
	for iso, name := range countryNames {
		if len(iso) != 2 || iso != strings.ToUpper(iso) {
			t.Errorf("код %q: ожидается ISO 3166-1 alpha-2 в верхнем регистре", iso)
		}
		if name == "" {
			t.Errorf("код %q: пустое название", iso)
		}
	}
	// В ISO 3166-1 сейчас 249 кодов, плюс XK от Nominatim. Точное число —
	// страховка от случайно снесённой строки при правке большого литерала.
	if len(countryNames) != 250 {
		t.Errorf("в словаре %d стран, ожидается 250", len(countryNames))
	}
}

// TestWeeklyStatsCountryNames — в сводке страны названы словами, а не кодами.
func TestWeeklyStatsCountryNames(t *testing.T) {
	st := &WeeklyStats{
		GeocodeRequests:  4,
		ByGeocodeCountry: []CountRow{{"RU", 87}, {"CY", 2}, {"ZZ", 1}},
	}
	now := time.Now()
	text := formatWeeklyStats(st, now.AddDate(0, 0, -7), now)
	if !strings.Contains(text, "Страны (в Nominatim): Россия 87, Кипр 2, ZZ 1") {
		t.Errorf("страны в сводке не названы: %q", text)
	}
}
