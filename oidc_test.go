package main

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// rsaJWKS отдаёт JWKS с одним публичным RSA-ключом (kid) — как это делают
// endpoint'ы Telegram/Apple/Google, но локально, чтобы проверять Verify без сети.
func rsaJWKS(pub *rsa.PublicKey, kid string) string {
	n := base64.RawURLEncoding.EncodeToString(pub.N.Bytes())
	e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes())
	return fmt.Sprintf(`{"keys":[{"kty":"RSA","use":"sig","alg":"RS256","kid":%q,"n":%q,"e":%q}]}`, kid, n, e)
}

const (
	testTgClientID     = "8705267895"
	testAppleClientID  = "net.nous.ether"
	testGoogleClientID = "123-abc.apps.googleusercontent.com"
	testKID            = "k1"
)

// authTestEnv — сервер со всеми тремя провайдерами, подписывающими токены одним
// локальным ключом. Реальные issuer'ы провайдеров сохранены: их проверка — часть
// того, что тестируем.
type authTestEnv struct {
	srv      *httptest.Server
	store    *Store
	key      *rsa.PrivateKey
	notified chan map[string]any
	t        *testing.T
}

func newAuthTestEnv(t *testing.T) *authTestEnv {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	body := rsaJWKS(&key.PublicKey, testKID)
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	}))
	t.Cleanup(jwks.Close)

	store := openTestStore(t)
	// уведомления в служебный канал с перехваченным транспортом (см. notify_test):
	// проверяем, что регистрация постит в канал, а повторный вход — нет
	notify, notified := newFakeNotifier()

	verifiers := map[string]*Verifier{
		ProviderTelegram: NewTelegramVerifier(testTgClientID, jwks.URL),
		ProviderApple:    NewAppleVerifier([]string{testAppleClientID}),
		ProviderGoogle:   NewGoogleVerifier([]string{testGoogleClientID}),
	}
	// у Apple/Google адрес JWKS настоящий — подменяем на локальный
	verifiers[ProviderApple].jwksURL = jwks.URL
	verifiers[ProviderGoogle].jwksURL = jwks.URL

	mux := http.NewServeMux()
	registerREST(mux, store, verifiers, notify)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &authTestEnv{srv: srv, store: store, key: key, notified: notified, t: t}
}

// sign выпускает ID-token от имени провайдера.
func (e *authTestEnv) sign(key *rsa.PrivateKey, iss, aud string, exp time.Time, claims oidcClaims) string {
	e.t.Helper()
	claims.Issuer = iss
	claims.Audience = jwt.ClaimStrings{aud}
	claims.ExpiresAt = jwt.NewNumericDate(exp)
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = testKID
	s, err := tok.SignedString(key)
	if err != nil {
		e.t.Fatalf("sign: %v", err)
	}
	return s
}

func (e *authTestEnv) post(provider string, req AuthRequest) (int, map[string]any) {
	e.t.Helper()
	resp, m := restPost(e.t, e.srv.URL+"/auth/"+provider, req)
	return resp.StatusCode, m
}

// POST /auth/telegram: корректно подписанный ID-token → сессия (и она реально
// резолвится); чужая подпись, чужой aud и протухший токен → 401 bad_auth.
func TestAuthTelegram(t *testing.T) {
	e := newAuthTestEnv(t)
	hour := time.Now().Add(time.Hour)

	valid := func(exp time.Time) oidcClaims {
		return oidcClaims{
			// sub у Telegram бывает opaque и вылезает за int64 — берём клейм id
			RegisteredClaims:  jwt.RegisteredClaims{Subject: "opaque-sub-larger-than-int64"},
			ID:                json.RawMessage("777"),
			PreferredUsername: "alex",
		}
	}

	code, m := e.post(ProviderTelegram, AuthRequest{
		IDToken: e.sign(e.key, tgIssuer, testTgClientID, hour, valid(hour)),
	})
	if code != http.StatusOK {
		t.Fatalf("auth(valid) = %d %v, want 200", code, m)
	}
	token, _ := m["token"].(string)
	if token == "" {
		t.Fatalf("auth(valid): пустой token, %v", m)
	}
	u, _ := m["user"].(map[string]any)
	if u == nil || u["name"] != "alex" {
		t.Fatalf("auth(valid): user = %v, want name=alex", m["user"])
	}
	// наружу уходит внутренний id, а не Telegram id
	if id, _ := u["id"].(float64); int64(id) == 777 {
		t.Fatalf("в ответе Telegram id (%v) — он не должен покидать базу", u["id"])
	}
	got, err := e.store.UserBySession(token)
	if err != nil || got == nil || got.TgUsername != "alex" {
		t.Fatalf("сессия из auth не резолвится: u=%v err=%v", got, err)
	}

	// регистрация → уведомление в служебный канал (шлётся в горутине)
	select {
	case n := <-e.notified:
		if text, _ := n["text"].(string); !strings.Contains(text, "Новый пользователь") ||
			!strings.Contains(text, "telegram") {
			t.Errorf("уведомление о регистрации: text = %q", text)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("уведомление о регистрации не отправлено")
	}

	// повторный вход тем же аккаунтом — это НЕ регистрация, в канал не пишем
	if code, m = e.post(ProviderTelegram, AuthRequest{
		IDToken: e.sign(e.key, tgIssuer, testTgClientID, hour, valid(hour)),
	}); code != http.StatusOK {
		t.Fatalf("auth(повторный) = %d %v, want 200", code, m)
	}
	select {
	case n := <-e.notified:
		t.Errorf("повторный вход прислал уведомление: %v", n["text"])
	case <-time.After(300 * time.Millisecond):
	}

	// чужая подпись (другой ключ, тот же kid) → 401
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	if code, m = e.post(ProviderTelegram, AuthRequest{
		IDToken: e.sign(other, tgIssuer, testTgClientID, hour, valid(hour)),
	}); code != http.StatusUnauthorized || m["code"] != "bad_auth" {
		t.Fatalf("auth(bad sig) = %d %v, want 401 bad_auth", code, m)
	}
	// чужой aud → 401
	if code, m = e.post(ProviderTelegram, AuthRequest{
		IDToken: e.sign(e.key, tgIssuer, "999", hour, valid(hour)),
	}); code != http.StatusUnauthorized || m["code"] != "bad_auth" {
		t.Fatalf("auth(bad aud) = %d %v, want 401 bad_auth", code, m)
	}
	// чужой issuer (токен Apple, присланный в /auth/telegram) → 401
	if code, m = e.post(ProviderTelegram, AuthRequest{
		IDToken: e.sign(e.key, appleIssuer, testTgClientID, hour, valid(hour)),
	}); code != http.StatusUnauthorized || m["code"] != "bad_auth" {
		t.Fatalf("auth(bad iss) = %d %v, want 401 bad_auth", code, m)
	}
	// протухший → 401
	past := time.Now().Add(-time.Hour)
	if code, m = e.post(ProviderTelegram, AuthRequest{
		IDToken: e.sign(e.key, tgIssuer, testTgClientID, past, valid(past)),
	}); code != http.StatusUnauthorized || m["code"] != "bad_auth" {
		t.Fatalf("auth(expired) = %d %v, want 401 bad_auth", code, m)
	}
}

// Sign in with Apple: в токене нет ни имени, ни фото — имя приходит рядом с
// токеном от клиента (Apple отдаёт его ОДИН раз, при первой авторизации), и при
// втором входе сохранённый профиль не должен обнулиться.
func TestAuthApple(t *testing.T) {
	e := newAuthTestEnv(t)
	hour := time.Now().Add(time.Hour)
	const sub = "000123.abcdef.1234"

	idToken := func() string {
		return e.sign(e.key, appleIssuer, testAppleClientID, hour, oidcClaims{
			RegisteredClaims: jwt.RegisteredClaims{Subject: sub},
		})
	}

	code, m := e.post(ProviderApple, AuthRequest{IDToken: idToken(), Name: "Мария Иванова"})
	if code != http.StatusOK {
		t.Fatalf("apple(first) = %d %v, want 200", code, m)
	}
	u, _ := m["user"].(map[string]any)
	if u == nil || u["name"] != "Мария Иванова" {
		t.Fatalf("apple(first): user = %v, want имя от клиента", m["user"])
	}
	if u["username"] != nil || u["avatar_url"] != nil {
		t.Fatalf("apple(first): у SIWA нет @username и фото, а в ответе они есть: %v", u)
	}

	// второй вход — клиент имени уже не знает; сервер обязан показать
	// сохранённое, а не пустое
	code, m = e.post(ProviderApple, AuthRequest{IDToken: idToken()})
	if code != http.StatusOK {
		t.Fatalf("apple(second) = %d %v, want 200", code, m)
	}
	if u, _ := m["user"].(map[string]any); u == nil || u["name"] != "Мария Иванова" {
		t.Fatalf("apple(second): имя потеряно: %v", m["user"])
	}

	// присланное клиентом имя обрезается и не содержит переводов строк —
	// иначе им можно испортить вёрстку ленты
	long := strings.Repeat("я", maxNameLen+20)
	code, m = e.post(ProviderApple, AuthRequest{
		IDToken: e.sign(e.key, appleIssuer, testAppleClientID, hour, oidcClaims{
			RegisteredClaims: jwt.RegisteredClaims{Subject: "sub-2"},
		}),
		Name: "ху\nлиган " + long,
	})
	if code != http.StatusOK {
		t.Fatalf("apple(long name) = %d %v", code, m)
	}
	name, _ := m["user"].(map[string]any)["name"].(string)
	if strings.Contains(name, "\n") || len([]rune(name)) > maxNameLen {
		t.Fatalf("имя не нормализовано: %q (%d рун)", name, len([]rune(name)))
	}
}

// Google: sub как идентификатор, имя и фото — из токена.
func TestAuthGoogle(t *testing.T) {
	e := newAuthTestEnv(t)
	hour := time.Now().Add(time.Hour)

	code, m := e.post(ProviderGoogle, AuthRequest{
		IDToken: e.sign(e.key, "https://accounts.google.com", testGoogleClientID, hour, oidcClaims{
			RegisteredClaims: jwt.RegisteredClaims{Subject: "g-1"},
			Name:             "Иван Петров",
			Picture:          "https://lh3.googleusercontent.com/a/x.jpg",
		}),
	})
	if code != http.StatusOK {
		t.Fatalf("google = %d %v, want 200", code, m)
	}
	u, _ := m["user"].(map[string]any)
	if u == nil || u["name"] != "Иван Петров" ||
		u["avatar_url"] != "https://lh3.googleusercontent.com/a/x.jpg" {
		t.Fatalf("google: user = %v", m["user"])
	}
	// второе написание issuer у Google тоже валидно
	if code, m = e.post(ProviderGoogle, AuthRequest{
		IDToken: e.sign(e.key, "accounts.google.com", testGoogleClientID, hour, oidcClaims{
			RegisteredClaims: jwt.RegisteredClaims{Subject: "g-2"},
			Name:             "Второй",
		}),
	}); code != http.StatusOK {
		t.Fatalf("google(issuer без схемы) = %d %v, want 200", code, m)
	}
}

// Незаданный в конфиге провайдер отвечает 501 с обычным JSON-телом, а не
// текстовым 404 от net/http: на 404 у клиента падал разбор ответа, и ошибка
// настройки выглядела в приложении как загадочное «Не удалось войти».
func TestAuthProviderNotConfigured(t *testing.T) {
	store := openTestStore(t)
	mux := http.NewServeMux()
	registerREST(mux, store, map[string]*Verifier{
		ProviderTelegram: NewTelegramVerifier("x", "http://127.0.0.1:0/jwks"),
	}, nil)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, body := restPost(t, srv.URL+"/auth/apple", AuthRequest{IDToken: "whatever"})
	if resp.StatusCode != http.StatusNotImplemented || body["code"] != "provider_disabled" {
		t.Fatalf("/auth/apple без конфига = %d %v, want 501 provider_disabled",
			resp.StatusCode, body)
	}
	if msg, _ := body["message"].(string); !strings.Contains(msg, "apple") {
		t.Fatalf("сообщение не называет провайдера: %q", msg)
	}
}

// Постоянный бан закрывает вход ДО создания аккаунта — иначе нарушитель удалял
// бы аккаунт и заводил новый тем же Apple ID.
func TestAuthBannedIdentity(t *testing.T) {
	e := newAuthTestEnv(t)
	hour := time.Now().Add(time.Hour)
	const sub = "banned-sub"

	idToken := e.sign(e.key, appleIssuer, testAppleClientID, hour, oidcClaims{
		RegisteredClaims: jwt.RegisteredClaims{Subject: sub},
	})
	if code, m := e.post(ProviderApple, AuthRequest{IDToken: idToken, Name: "Нарушитель"}); code != http.StatusOK {
		t.Fatalf("первый вход = %d %v", code, m)
	}
	<-e.notified // регистрация

	// модератор блокирует навсегда: аккаунт удаляется, запись о бане остаётся
	var userID int64
	if err := e.store.db.QueryRow(
		`SELECT user_id FROM identities WHERE provider = ? AND provider_uid = ?`,
		ProviderApple, sub).Scan(&userID); err != nil {
		t.Fatalf("найти личность: %v", err)
	}
	if _, err := e.store.BanPermanent(userID, "спам"); err != nil {
		t.Fatalf("block: %v", err)
	}

	code, m := e.post(ProviderApple, AuthRequest{IDToken: idToken, Name: "Нарушитель"})
	if code != http.StatusForbidden || m["code"] != "banned" {
		t.Fatalf("вход забаненного = %d %v, want 403 banned", code, m)
	}
	// и аккаунт не воссоздан
	var n int
	if err := e.store.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("забаненный вход создал %d аккаунтов", n)
	}
}

// Имя можно задать вручную: без этого аккаунт, зарегистрированный через Apple без
// имени (оно приходит только при первой авторизации и вне токена), навсегда
// остался бы безымянным — починить его нечем, кроме как спросить человека.
func TestSetName(t *testing.T) {
	store := openTestStore(t)
	mux := http.NewServeMux()
	registerREST(mux, store, map[string]*Verifier{}, nil)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// аккаунт без имени — как после входа через Apple, когда имя не доехало
	id, _, _, err := store.UpsertByIdentity(ProviderUser{Provider: ProviderApple, UID: "sub-noname"})
	if err != nil {
		t.Fatalf("создать аккаунт: %v", err)
	}
	token, err := store.NewSession(id)
	if err != nil {
		t.Fatalf("сессия: %v", err)
	}

	// без сессии не пускаем
	resp, body := restPost(t, srv.URL+"/profile/name", SetNameData{Token: "garbage", Name: "Кто-то"})
	if resp.StatusCode != http.StatusUnauthorized || body["code"] != "bad_session" {
		t.Fatalf("чужой токен = %d %v, want 401 bad_session", resp.StatusCode, body)
	}
	// пустое имя — не имя
	for _, empty := range []string{"", "   ", "\n"} {
		resp, body = restPost(t, srv.URL+"/profile/name", SetNameData{Token: token, Name: empty})
		if resp.StatusCode != http.StatusBadRequest || body["code"] != "bad_data" {
			t.Fatalf("имя %q = %d %v, want 400 bad_data", empty, resp.StatusCode, body)
		}
	}

	// нормальный случай: имя сохранено и вернулось в ответе
	resp, body = restPost(t, srv.URL+"/profile/name", SetNameData{Token: token, Name: "  Мария  "})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("set name = %d %v, want 200", resp.StatusCode, body)
	}
	if u, _ := body["user"].(map[string]any); u == nil || u["name"] != "Мария" {
		t.Fatalf("ответ: %v, want имя без лишних пробелов", body["user"])
	}
	if u, err := store.UserByID(id); err != nil || u == nil || u.FullName != "Мария" {
		t.Fatalf("в БД имя = %+v err=%v", u, err)
	}

	// Непустое имя провайдер не перезаписывает: раз оно есть — оно уже чьё-то.
	// Иначе тот, кто ввёл имя на экране онбординга, терял бы его при следующем
	// входе через провайдера.
	if _, _, _, err := store.UpsertByIdentity(ProviderUser{
		Provider: ProviderApple, UID: "sub-noname", Name: "Имя От Провайдера",
	}); err != nil {
		t.Fatalf("повторный вход: %v", err)
	}
	if u, _ := store.UserByID(id); u == nil || u.FullName != "Мария" {
		t.Fatalf("провайдер перезаписал заданное вручную имя: %+v", u)
	}

	// присланное клиентом имя нормализуется так же, как при входе: без переводов
	// строк и не длиннее maxNameLen — иначе им можно испортить вёрстку ленты
	long := strings.Repeat("я", maxNameLen+10)
	if resp, body = restPost(t, srv.URL+"/profile/name",
		SetNameData{Token: token, Name: "мно\nго " + long}); resp.StatusCode != http.StatusOK {
		t.Fatalf("длинное имя = %d %v", resp.StatusCode, body)
	}
	got, _ := body["user"].(map[string]any)["name"].(string)
	if strings.Contains(got, "\n") || len([]rune(got)) > maxNameLen {
		t.Fatalf("имя не нормализовано: %q (%d рун)", got, len([]rune(got)))
	}
}

// Привязка второго способа входа: аккаунт, созданный через Apple (без
// @username и аватара), привязывает Telegram — и получает их, оставаясь тем же
// аккаунтом. Слияния аккаунтов нет: занятая личность отбивается 409.
func TestLinkIdentity(t *testing.T) {
	e := newAuthTestEnv(t)
	hour := time.Now().Add(time.Hour)

	appleToken := e.sign(e.key, appleIssuer, testAppleClientID, hour, oidcClaims{
		RegisteredClaims: jwt.RegisteredClaims{Subject: "sub-linker"},
	})
	code, m := e.post(ProviderApple, AuthRequest{IDToken: appleToken, Name: "Мария"})
	if code != http.StatusOK {
		t.Fatalf("вход через Apple = %d %v", code, m)
	}
	<-e.notified // регистрация
	session, _ := m["token"].(string)
	user, _ := m["user"].(map[string]any)
	if got := user["providers"]; fmt.Sprint(got) != "[apple]" {
		t.Fatalf("providers после входа = %v, want [apple]", got)
	}

	tgClaims := func(id string) oidcClaims {
		return oidcClaims{
			ID:                json.RawMessage(id),
			PreferredUsername: "alex_tg",
			Picture:           "https://t.me/i/a.jpg",
		}
	}
	link := func(body LinkRequest) (int, map[string]any) {
		resp, out := restPost(t, e.srv.URL+"/profile/link/telegram", body)
		return resp.StatusCode, out
	}

	// привязка: аккаунт тот же, но появились @username и аватар
	code, m = link(LinkRequest{
		Token:   session,
		IDToken: e.sign(e.key, tgIssuer, testTgClientID, hour, tgClaims("555")),
	})
	if code != http.StatusOK {
		t.Fatalf("привязка = %d %v, want 200", code, m)
	}
	u, _ := m["user"].(map[string]any)
	if u["username"] != "alex_tg" || u["avatar_url"] != "https://t.me/i/a.jpg" {
		t.Fatalf("привязка не дозаполнила профиль: %v", u)
	}
	if u["name"] != "Мария" {
		t.Fatalf("привязка перезаписала имя: %v", u["name"])
	}
	if got := fmt.Sprint(u["providers"]); got != "[apple telegram]" {
		t.Fatalf("providers = %v, want [apple telegram]", got)
	}

	// повторная привязка той же личности — не ошибка
	if code, m = link(LinkRequest{
		Token:   session,
		IDToken: e.sign(e.key, tgIssuer, testTgClientID, hour, tgClaims("555")),
	}); code != http.StatusOK {
		t.Fatalf("повторная привязка = %d %v, want 200", code, m)
	}

	// личность, занятая другим аккаунтом → 409, без слияния
	if _, _, _, err := e.store.UpsertByIdentity(ProviderUser{
		Provider: ProviderTelegram, UID: "999", Name: "Другой",
	}); err != nil {
		t.Fatalf("второй аккаунт: %v", err)
	}
	code, m = link(LinkRequest{
		Token:   session,
		IDToken: e.sign(e.key, tgIssuer, testTgClientID, hour, tgClaims("999")),
	})
	if code != http.StatusConflict || m["code"] != "identity_taken" {
		t.Fatalf("занятая личность = %d %v, want 409 identity_taken", code, m)
	}

	// мусорная сессия и мусорный токен провайдера
	if code, m = link(LinkRequest{Token: "garbage", IDToken: "x"}); code != http.StatusUnauthorized ||
		m["code"] != "bad_session" {
		t.Fatalf("чужая сессия = %d %v, want 401 bad_session", code, m)
	}
	if code, m = link(LinkRequest{Token: session, IDToken: "not-a-jwt"}); code != http.StatusUnauthorized ||
		m["code"] != "bad_auth" {
		t.Fatalf("мусорный id_token = %d %v, want 401 bad_auth", code, m)
	}
}
