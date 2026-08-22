package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// released — дата выхода latest в тестах; softDelay считаем от неё.
var released = time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)

// full — заполненное правило: в конфиге все три поля обязательны, поэтому
// отправной точкой любого теста служит валидный блок.
func full() ClientVersionRule {
	return ClientVersionRule{Min: "1.1.0", Latest: "1.3.0", LatestSince: "2026-08-22"}
}

// without — валидное правило с одним испорченным полем: так в таблице невалидных
// конфигов видно ровно то, что проверяется, а не весь блок целиком.
func without(break_ func(*ClientVersionRule)) ClientVersionRule {
	r := full()
	break_(&r)
	return r
}

func testGate(t *testing.T, rules map[string]ClientVersionRule) *versionGate {
	t.Helper()
	g, err := newVersionGate(rules)
	if err != nil {
		t.Fatalf("newVersionGate: %v", err)
	}
	return g
}

// TestVersionVerdict — ядро механизма. Ошибка здесь либо блокирует людей на
// рабочей сборке, либо молчит о нерабочей.
func TestVersionVerdict(t *testing.T) {
	gate := testGate(t, map[string]ClientVersionRule{
		platformIOS: full(),
	})

	week := released.Add(softDelay)
	cases := []struct {
		name     string
		platform string
		version  string
		now      time.Time
		want     string
	}{
		{"ниже min — обновиться обязательно", platformIOS, "1.0.0", week, updateRequired},
		{"ровно min — уже не required", platformIOS, "1.1.0", week, updateSoft},
		{"старее latest, неделя прошла", platformIOS, "1.2.0", week, updateSoft},
		{"старее latest, но неделя ещё нет", platformIOS, "1.2.0", week.Add(-time.Second), updateOK},
		{"ровно latest", platformIOS, "1.3.0", week, updateOK},
		{"новее latest — dev-сборка", platformIOS, "1.4.0", week, updateOK},
		// min важнее задержки: сборка объявлена неработающей, ждать нечего
		{"ниже min до истечения недели", platformIOS, "1.0.0", released, updateRequired},
		{"платформы нет в конфиге", platformAndroid, "0.1.0", week, updateOK},
		{"версия не разобралась", platformIOS, "1.2", week, updateOK},
		{"версия пустая", platformIOS, "", week, updateOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := gate.verdict(c.platform, c.version, c.now).Status; got != c.want {
				t.Errorf("verdict(%q, %q) = %q, want %q", c.platform, c.version, got, c.want)
			}
		})
	}
}

// TestVersionVerdictURL — url отдаём только когда обновляться есть зачем: кнопка
// «Обновить» существует ровно в этих двух состояниях.
func TestVersionVerdictURL(t *testing.T) {
	gate := testGate(t, map[string]ClientVersionRule{
		platformIOS:     full(),
		platformAndroid: full(),
	})
	week := released.Add(softDelay)

	if got := gate.verdict(platformIOS, "1.0.0", week); got.URL != appStoreURL {
		t.Errorf("ios url = %q, want %q", got.URL, appStoreURL)
	}
	if got := gate.verdict(platformAndroid, "1.0.0", week); got.URL != playURL {
		t.Errorf("android url = %q, want %q", got.URL, playURL)
	}
	if got := gate.verdict(platformIOS, "1.3.0", week); got.URL != "" {
		t.Errorf("url при ok = %q, want пусто", got.URL)
	}
	// latest отдаём всегда: клиенту его показывать человеку, даже когда всё ок
	if got := gate.verdict(platformIOS, "1.3.0", week); got.Latest != "1.3.0" {
		t.Errorf("latest = %q, want 1.3.0", got.Latest)
	}
}

// TestVersionGateNil — конфига без client_versions достаточно для работы: сервер
// поднимается и про обновления молчит.
func TestVersionGateNil(t *testing.T) {
	var gate *versionGate
	if got := gate.verdict(platformIOS, "0.0.1", released).Status; got != updateOK {
		t.Errorf("status = %q, want ok", got)
	}
	empty := testGate(t, nil)
	if got := empty.verdict(platformIOS, "0.0.1", released).Status; got != updateOK {
		t.Errorf("status пустого gate = %q, want ok", got)
	}
}

// TestVersionGateRejectsBadConfig — неверный порог хуже отсутствующего: он тихо
// блокирует людей или тихо ничего не делает. Поэтому — ошибка старта.
func TestVersionGateRejectsBadConfig(t *testing.T) {
	cases := []struct {
		name     string
		platform string
		rule     ClientVersionRule
	}{
		{"неизвестная платформа", "windows", full()},
		{"min не задан", platformIOS, without(func(r *ClientVersionRule) { r.Min = "" })},
		{"latest не задан", platformIOS, without(func(r *ClientVersionRule) { r.Latest = "" })},
		{"latest_since не задан", platformIOS, without(func(r *ClientVersionRule) { r.LatestSince = "" })},
		{"min выше latest", platformIOS, without(func(r *ClientVersionRule) { r.Min = "2.0.0" })},
		{"min не версия", platformIOS, without(func(r *ClientVersionRule) { r.Min = "1.x" })},
		{"latest не версия", platformIOS, without(func(r *ClientVersionRule) { r.Latest = "1" })},
		{"версия из двух чисел", platformIOS, without(func(r *ClientVersionRule) { r.Min = "1.2" })},
		{"дата не дата", platformIOS, without(func(r *ClientVersionRule) { r.LatestSince = "22.08.2026" })},
		{"дата с временем", platformIOS, without(func(r *ClientVersionRule) { r.LatestSince = "2026-08-22T10:00:00Z" })},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := newVersionGate(map[string]ClientVersionRule{c.platform: c.rule}); err == nil {
				t.Error("ошибки нет, а конфиг невалидный")
			}
		})
	}
}

// TestParseSemver — разбор версии. «1.2» не прощаем: гадать, что имелось в виду,
// хуже, чем ответить ok и не трогать человека.
func TestParseSemver(t *testing.T) {
	ok := map[string]semver{
		"1.2.3":    {1, 2, 3},
		"0.0.1":    {0, 0, 1},
		" 1.2.3 ":  {1, 2, 3},
		"10.20.30": {10, 20, 30},
	}
	for in, want := range ok {
		if got, valid := parseSemver(in); !valid || got != want {
			t.Errorf("parseSemver(%q) = %v, %v; want %v, true", in, got, valid, want)
		}
	}
	for _, in := range []string{"", "1", "1.2", "1.2.3.4", "1.2.x", "-1.0.0", "v1.2.3", "1.2.3-beta"} {
		if _, valid := parseSemver(in); valid {
			t.Errorf("parseSemver(%q) — разобралось, а не должно", in)
		}
	}
}

// TestVersionEndpoint — GET /version отвечает вердиктом и пишет отметку о
// версии. Отметка — единственный источник ответа на «остались ли живые старые
// сборки», поэтому проверяем и её.
func TestVersionEndpoint(t *testing.T) {
	store := openTestStore(t)
	gate := testGate(t, map[string]ClientVersionRule{
		platformIOS: without(func(r *ClientVersionRule) { r.LatestSince = "2020-01-01" }),
	})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /version", handleVersion(store, gate))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	var got UpdateData
	resp, err := http.Get(srv.URL + "/version?platform=ios&version=1.0.0")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Status != updateRequired || got.URL != appStoreURL {
		t.Errorf("ответ = %+v, want required + ссылка в App Store", got)
	}
	if n := versionRowCount(t, store); n != 1 {
		t.Errorf("client_version = %d строк, want 1", n)
	}
}

// TestVersionEndpointSkipsJunk — эндпоинт открытый и без входа, а по таблице мы
// решаем судьбу старых сборок: мусор в ней исказил бы именно это решение.
func TestVersionEndpointSkipsJunk(t *testing.T) {
	store := openTestStore(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /version", handleVersion(store, testGate(t, nil)))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	for _, q := range []string{
		"",                                // без параметров вовсе
		"?platform=ios",                   // без версии
		"?platform=ios&version=1.2",       // версия не разбирается
		"?platform=windows&version=1.2.0", // платформа не наша
		"?platform=&version=1.2.0",        // платформа пустая
	} {
		resp, err := http.Get(srv.URL + "/version" + q)
		if err != nil {
			t.Fatalf("get %q: %v", q, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%q: status = %d, want 200", q, resp.StatusCode)
		}
	}
	if n := versionRowCount(t, store); n != 0 {
		t.Errorf("client_version = %d строк, want 0", n)
	}
}

// TestVersionEndpointRejectsPost — это чтение, и метод должен быть GET. Отбивает
// сам роутер (метод указан в паттерне), поэтому ответ — текстовый 405 от
// стандартной библиотеки, а не наш JSON.
func TestVersionEndpointRejectsPost(t *testing.T) {
	store := openTestStore(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /version", handleVersion(store, testGate(t, nil)))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/version", "application/json", nil)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}
}

// TestWeeklyStatsByClientVersion — распределение версий попадает в сводку: без
// него отметки бесполезны, потому что смотреть на них негде.
func TestWeeklyStatsByClientVersion(t *testing.T) {
	store := openTestStore(t)
	for _, v := range []string{"1.2.0", "1.2.0", "1.3.0"} {
		if err := store.SaveClientVersion(platformIOS, v); err != nil {
			t.Fatalf("сохранение версии: %v", err)
		}
	}
	now := time.Now()
	st, err := store.WeeklyStats(now.Add(-time.Hour).UnixMilli(), now.Add(time.Hour).UnixMilli())
	if err != nil {
		t.Fatalf("weekly stats: %v", err)
	}
	if got := groupCount(st.ByClientVersion, "ios 1.2.0"); got != 2 {
		t.Errorf("ios 1.2.0 = %d, want 2", got)
	}
	if got := groupCount(st.ByClientVersion, "ios 1.3.0"); got != 1 {
		t.Errorf("ios 1.3.0 = %d, want 1", got)
	}

	text := formatWeeklyStats(st, now.AddDate(0, 0, -7), now)
	if !strings.Contains(text, "Версии: ios 1.2.0 2") {
		t.Errorf("в сводке нет распределения версий: %q", text)
	}
}

func versionRowCount(t *testing.T, store *Store) int {
	t.Helper()
	var n int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM client_version`).Scan(&n); err != nil {
		t.Fatalf("count client_version: %v", err)
	}
	return n
}
