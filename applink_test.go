package main

import (
	"database/sql"
	"net/http"
	"net/url"
	"testing"
)

// UA живых браузеров — по ним хендлер выбирает стор.
const (
	uaIPhone  = "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1"
	uaAndroid = "Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Mobile Safari/537.36"
	uaDesktop = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"
)

// appAccessRow — последняя строка app_access; uid nullable, поэтому sql.NullInt64.
type appAccessRow struct {
	UID      sql.NullInt64
	Src      string
	Platform string
	Outcome  string
	UA       string
	Lang     string
	IP       string
}

func lastAppAccess(t *testing.T, store *Store) appAccessRow {
	t.Helper()
	var r appAccessRow
	err := store.db.QueryRow(`
		SELECT uid, src, platform, outcome, ua, lang, ip
		FROM app_access ORDER BY id DESC LIMIT 1`).
		Scan(&r.UID, &r.Src, &r.Platform, &r.Outcome, &r.UA, &r.Lang, &r.IP)
	if err != nil {
		t.Fatalf("read app_access: %v", err)
	}
	return r
}

func appAccessCount(t *testing.T, store *Store) int {
	t.Helper()
	var n int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM app_access`).Scan(&n); err != nil {
		t.Fatalf("count app_access: %v", err)
	}
	return n
}

// noRedirectClient — редирект нужно увидеть, а не пройти: цель ведёт в App Store.
func noRedirectClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// withAppleProviderToken подставляет `pt` на время теста. В проде это постоянное
// значение аккаунта, но пока оно не вписано, без подмены не проверить ветку
// полной campaign-ссылки — той самой, ошибку в которой Apple молча проглотит.
func withAppleProviderToken(t *testing.T, token string) {
	t.Helper()
	prev := appleProviderToken
	appleProviderToken = token
	t.Cleanup(func() { appleProviderToken = prev })
}

// getAppLink дёргает /app с заданным User-Agent и возвращает ответ.
func getAppLink(t *testing.T, srv, url, ua string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv+url, nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept-Language", "ru-RU,ru;q=0.9")
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("get %s: %v", url, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// TestAppLinkIOS — основной сценарий приглашения: iPhone уходит в App Store, а в
// app_access появляется строка с приглашающим и источником ссылки.
func TestAppLinkIOS(t *testing.T) {
	srv, store := newTestServer(t)

	resp := getAppLink(t, srv.URL, "/app?src=apli&uid=42", uaIPhone)

	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want 302", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != appStoreLink("apli") {
		t.Errorf("Location = %q, want %q", got, appStoreLink("apli"))
	}
	// закэшированный редирект до сервера не дойдёт и не посчитается
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}

	row := lastAppAccess(t, store)
	if !row.UID.Valid || row.UID.Int64 != 42 {
		t.Errorf("uid = %v, want 42", row.UID)
	}
	if row.Src != "apli" {
		t.Errorf("src = %q, want apli", row.Src)
	}
	if row.Platform != platformIOS || row.Outcome != outcomeAppStore {
		t.Errorf("platform/outcome = %q/%q, want ios/appstore", row.Platform, row.Outcome)
	}
	if row.UA != uaIPhone {
		t.Errorf("ua = %q, want сырой User-Agent", row.UA)
	}
	if row.Lang != "ru-RU,ru;q=0.9" {
		t.Errorf("lang = %q", row.Lang)
	}
}

// TestAppLinkAndroidGoesToPlay — приложение опубликовано в Google Play, значит
// Android уходит в стор, а не на лендинг (так было, пока playURL пустовал).
// Важно, что platform и outcome остаются разными полями: platform — догадка по
// User-Agent, outcome — записанное решение сервера. Старые строки со времён
// заглушки так и лежат как android/landing и остаются читаемыми.
func TestAppLinkAndroidGoesToPlay(t *testing.T) {
	srv, store := newTestServer(t)

	resp := getAppLink(t, srv.URL, "/app?src=apqr&uid=7", uaAndroid)

	if got := resp.Header.Get("Location"); got != playLink("apqr") {
		t.Errorf("Location = %q, want %q", got, playLink("apqr"))
	}
	row := lastAppAccess(t, store)
	if row.Platform != platformAndroid {
		t.Errorf("platform = %q, want android", row.Platform)
	}
	if row.Outcome != outcomePlay {
		t.Errorf("outcome = %q, want play", row.Outcome)
	}
	if row.Src != "apqr" {
		t.Errorf("src = %q, want apqr", row.Src)
	}
}

// TestAppStoreLinkCampaign — метка кампании клеится к ссылке App Store только
// вместе с pt: Apple засчитывает переход лишь при обоих параметрах, а одинокий
// ct игнорирует, поэтому неполную ссылку не собираем вовсе.
func TestAppStoreLinkCampaign(t *testing.T) {
	if got := appStoreLink(""); got != appStoreURL {
		t.Errorf("без метки = %q, want %q", got, appStoreURL)
	}
	if got := appStoreLink("st0"); got != appStoreURL {
		t.Errorf("pt не задан = %q, want %q (ct в одиночку бесполезен)", got, appStoreURL)
	}

	withAppleProviderToken(t, "123456")
	want := appStoreURL + "?pt=123456&ct=st0&mt=8"
	if got := appStoreLink("st0"); got != want {
		t.Errorf("= %q, want %q", got, want)
	}
}

// TestPlayLinkEncodesReferrer — utm-строка внутри referrer закодирована целиком.
// Незакодированный `&` разорвал бы её: части стали бы параметрами самой ссылки
// Play, referrer приехал бы обрезанным, и источник в отчётах не появился бы.
func TestPlayLinkEncodesReferrer(t *testing.T) {
	if got := playLink(""); got != playURL {
		t.Errorf("без метки = %q, want %q", got, playURL)
	}
	want := playURL + "&referrer=utm_source%3Dst0%26utm_medium%3Dapplink"
	if got := playLink("st0"); got != want {
		t.Errorf("= %q, want %q", got, want)
	}
}

// TestAppLinkStickerCampaign — сквозной путь печатной метки: со стикера человек
// уезжает в стор вместе с меткой, и она же ложится в app_access. На стикере при
// этом напечатан простой адрес (`/app?src=st0`, без uid — звал не человек, а
// наклейка); campaign-параметры дописывает сервер.
func TestAppLinkStickerCampaign(t *testing.T) {
	withAppleProviderToken(t, "123456")
	srv, store := newTestServer(t)

	resp := getAppLink(t, srv.URL, "/app?src=st0", uaIPhone)
	if got, want := resp.Header.Get("Location"), appStoreURL+"?pt=123456&ct=st0&mt=8"; got != want {
		t.Errorf("Location = %q, want %q", got, want)
	}

	row := lastAppAccess(t, store)
	if row.Src != "st0" {
		t.Errorf("src = %q, want st0", row.Src)
	}
	if row.UID.Valid {
		t.Errorf("uid = %v, want NULL", row.UID.Int64)
	}
}

// TestAppLinkCampaignRejectsJunk — src приходит из открытого эндпоинта и уезжает
// в заголовок Location, поэтому в стор попадают только ascii-слаги. Мусор метки
// не получает, НО в статистику ложится как есть: терять строку из-за неудачной
// атрибуции неправильно — переход-то был.
//
// Первый случай — тот, ради которого здесь whitelist: `&` в метке дописал бы к
// ссылке стора чужие параметры.
func TestAppLinkCampaignRejectsJunk(t *testing.T) {
	withAppleProviderToken(t, "123456")
	srv, store := newTestServer(t)

	for _, src := range []string{"st0&ct=hijack", "st 0", "квартал", "st0#frag"} {
		before := appAccessCount(t, store)
		resp := getAppLink(t, srv.URL, "/app?src="+url.QueryEscape(src), uaIPhone)

		if got := resp.Header.Get("Location"); got != appStoreURL {
			t.Errorf("src=%q: Location = %q, want голый %q", src, got, appStoreURL)
		}
		if got := appAccessCount(t, store); got != before+1 {
			t.Errorf("src=%q: строк %d, want %d — переход обязан посчитаться", src, got, before+1)
		}
		if got := lastAppAccess(t, store).Src; got != src {
			t.Errorf("src = %q, want %q — в базу метка идёт как есть", got, src)
		}
	}
}

// TestAppLinkWithoutInviter — ссылку открыли без приглашения (или с мусором
// вместо uid). Переход обязан работать, а uid лечь в базу как NULL: 0 не бывает
// валидным users.id, и в отчётах NULL честнее.
func TestAppLinkWithoutInviter(t *testing.T) {
	srv, store := newTestServer(t)

	for _, url := range []string{"/app", "/app?uid=", "/app?uid=не-число"} {
		resp := getAppLink(t, srv.URL, url, uaDesktop)
		if resp.StatusCode != http.StatusFound {
			t.Fatalf("%s: status = %d, want 302", url, resp.StatusCode)
		}
		row := lastAppAccess(t, store)
		if row.UID.Valid {
			t.Errorf("%s: uid = %v, want NULL", url, row.UID.Int64)
		}
		if row.Platform != platformDesktop || row.Outcome != outcomeLanding {
			t.Errorf("%s: platform/outcome = %q/%q, want desktop/landing", url, row.Platform, row.Outcome)
		}
	}
}

// TestAppLinkClientIP — на проде запрос приходит через свой Caddy, и настоящий
// адрес лежит в X-Forwarded-For. Берётся последний элемент: начало списка
// клиент подделывает тривиально, а последним заголовок дописал наш прокси.
func TestAppLinkClientIP(t *testing.T) {
	srv, store := newTestServer(t)

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/app", nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("User-Agent", uaIPhone)
	req.Header.Set("X-Forwarded-For", "9.9.9.9, 203.0.113.7")
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	if got := lastAppAccess(t, store).IP; got != "203.0.113.7" {
		t.Errorf("ip = %q, want 203.0.113.7 (последний элемент XFF)", got)
	}
}

// TestAppLinkTruncatesInput — эндпоинт открытый, и всё присланное попадает в
// базу: длинные строки режутся.
func TestAppLinkTruncatesInput(t *testing.T) {
	srv, store := newTestServer(t)

	long := make([]byte, 200)
	for i := range long {
		long[i] = 'x'
	}
	getAppLink(t, srv.URL, "/app?src="+string(long), uaIPhone)

	if got := len(lastAppAccess(t, store).Src); got != maxSrcLen {
		t.Errorf("len(src) = %d, want %d", got, maxSrcLen)
	}
}

// TestAppLinkSkipsDeployCheck — проверка деплоя (ether-web/deploy.sh) дёргает
// /app после каждой выкатки. Редирект она получить обязана, а вот в статистику
// попадать не должна: это наш же curl, а не человек.
func TestAppLinkSkipsDeployCheck(t *testing.T) {
	withAppleProviderToken(t, "123456")
	srv, store := newTestServer(t)

	resp := getAppLink(t, srv.URL, "/app?src="+srcDeployCheck, uaIPhone)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusFound {
		t.Errorf("status = %d, want 302", resp.StatusCode)
	}
	if n := appAccessCount(t, store); n != 0 {
		t.Errorf("app_access = %d строк, want 0", n)
	}
	// И в отчётах сторов её тоже быть не должно: метка ушла бы туда кампанией,
	// хотя это наш собственный curl после выкатки.
	if got := resp.Header.Get("Location"); got != appStoreURL {
		t.Errorf("Location = %q, want голый %q", got, appStoreURL)
	}
}

// TestAppLinkRejectsPost — /app отвечает редиректом браузеру; остальные методы
// сюда не ходят и в статистику попадать не должны.
func TestAppLinkRejectsPost(t *testing.T) {
	srv, store := newTestServer(t)

	resp, err := noRedirectClient().Post(srv.URL+"/app", "application/json", nil)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}
	if n := appAccessCount(t, store); n != 0 {
		t.Errorf("app_access = %d строк, want 0", n)
	}
}

// TestDetectPlatform — разбор UA. Точность здесь не самоцель (сырой UA лежит в
// базе и его можно перечитать), но выбор стора зависит именно от неё.
func TestDetectPlatform(t *testing.T) {
	cases := []struct {
		name string
		ua   string
		want string
	}{
		{"iphone", uaIPhone, platformIOS},
		{"android", uaAndroid, platformAndroid},
		{"macos", uaDesktop, platformDesktop},
		{"windows", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) Chrome/126.0", platformDesktop},
		// Chrome на Android содержит и "Linux", и "Android" — победить должен Android,
		// иначе телефоны уезжали бы на лендинг вместо стора.
		{"android важнее linux", "Mozilla/5.0 (Linux; Android 14) Chrome/126.0", platformAndroid},
		{"ipad", "Mozilla/5.0 (iPad; CPU OS 17_5 like Mac OS X) Safari/604.1", platformIOS},
		// превью ссылки в мессенджере: узнаётся по unknown и фильтруется в отчётах
		{"бот превью", "TelegramBot (like TwitterBot)", platformUnknown},
		{"пусто", "", platformUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := detectPlatform(c.ua); got != c.want {
				t.Errorf("detectPlatform(%q) = %q, want %q", c.ua, got, c.want)
			}
		})
	}
}
