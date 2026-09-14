package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// geocodeServer — эндпоинт на заданном геокодере и лимите. Лимит задаётся
// тестом: прод-тир (десять запросов) пришлось бы выбирать циклом, а проверяем мы
// не число, а сам факт отсечки.
func geocodeServer(t *testing.T, geo Geocoder, lim messageLimit) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /geocode", handleGeocode(geo, newIPLimiter(lim)))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestGeocodeEndpoint — точка превращается в набор каналов. Это тот же ответ,
// что кадр located на WS: лендинг и приложение обязаны показывать одни каналы.
func TestGeocodeEndpoint(t *testing.T) {
	srv := geocodeServer(t, StubGeocoder{}, geocodeIPLimit)

	resp, err := http.Get(srv.URL + "/geocode?lat=55.791&lng=37.529")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got LocatedData
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Channels) != 5 || got.Channels[0].ID != PlanetChannel.ID {
		t.Fatalf("каналы = %+v, want набор broad→specific с планетой первой", got.Channels)
	}
	// Счётчик подписчиков здесь не считается вовсе, и ноль по контракту Channel
	// значит «не посчитали» — клиент такой канал показывает без числа.
	for _, ch := range got.Channels {
		if ch.Subscribers != 0 {
			t.Errorf("%s: subscribers = %d, want 0", ch.ID, ch.Subscribers)
		}
	}
}

// TestGeocodeEndpointBadCoords — мусор в параметрах до геокодера не доходит:
// иначе он ушёл бы в ключ кеша, в статистику и в запрос к Nominatim. NaN и Inf
// здесь не экзотика, а то, что ParseFloat разбирает успешно.
func TestGeocodeEndpointBadCoords(t *testing.T) {
	geo := &countingGeocoder{}
	srv := geocodeServer(t, geo, geocodeIPLimit)

	for _, q := range []string{
		"",                           // без параметров вовсе
		"?lat=55.791",                // без долготы
		"?lat=&lng=",                 // пустые
		"?lat=север&lng=37.529",      // не число
		"?lat=91&lng=37.529",         // за полюсом
		"?lat=55.791&lng=-181",       // за меридианом
		"?lat=NaN&lng=37.529",        // разбирается, но координатой не является
		"?lat=55.791&lng=Inf",        // то же самое
		"?lat=55.791&lng=37.529junk", // хвост после числа
	} {
		resp, err := http.Get(srv.URL + "/geocode" + q)
		if err != nil {
			t.Fatalf("get %q: %v", q, err)
		}
		var body map[string]any
		json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest || body["code"] != "bad_data" {
			t.Errorf("%q: = %d %v, want 400 bad_data", q, resp.StatusCode, body)
		}
	}
	if geo.count() != 0 {
		t.Errorf("геокодер позвали %d раз, want 0", geo.count())
	}
}

// TestGeocodeEndpointRateLimit — эндпоинт публичный и анонимный, а за ним
// внешний Nominatim с лимитом 1 req/s, очередь к которому общая с живыми
// пользователями. Поэтому лишний запрос отбивается, а не встаёт в неё.
func TestGeocodeEndpointRateLimit(t *testing.T) {
	geo := &countingGeocoder{}
	srv := geocodeServer(t, geo, messageLimit{capacity: 2, refill: time.Minute})

	for i := range 2 {
		resp, err := http.Get(srv.URL + "/geocode?lat=55.791&lng=37.529")
		if err != nil {
			t.Fatalf("get %d: %v", i, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("запрос %d: status = %d, want 200", i, resp.StatusCode)
		}
	}

	resp, err := http.Get(srv.URL + "/geocode?lat=55.791&lng=37.529")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	var body map[string]any
	json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests || body["code"] != "too_fast" {
		t.Fatalf("третий запрос = %d %v, want 429 too_fast", resp.StatusCode, body)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("нет Retry-After: отказ без срока клиенту нечем отработать")
	}
	// Отбитый запрос до геокодера не дошёл — ради этого лимит и стоит.
	if geo.count() != 2 {
		t.Errorf("геокодер позвали %d раз, want 2", geo.count())
	}
}

// TestGeocodeEndpointUpstreamFails — сбой Nominatim это 502, а не 500: отвечает
// не наш сервер, а тот, к кому он сходил.
func TestGeocodeEndpointUpstreamFails(t *testing.T) {
	geo := &countingGeocoder{err: errors.New("nominatim молчит")}
	srv := geocodeServer(t, geo, geocodeIPLimit)

	resp, err := http.Get(srv.URL + "/geocode?lat=55.791&lng=37.529")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	var body map[string]any
	json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway || body["code"] != "geocode_failed" {
		t.Fatalf("= %d %v, want 502 geocode_failed", resp.StatusCode, body)
	}
}
