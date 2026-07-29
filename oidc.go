package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"
)

// Проверка входа через провайдеров OIDC: Telegram, Apple (Sign in with Apple),
// Google. Механика у всех одна — клиент получает на устройстве подписанный
// ID-token (JWT) и присылает его нам, а сервер проверяет подпись по ПУБЛИЧНЫМ
// ключам провайдера (JWKS) и клеймы iss/aud/exp. Никаких секретов провайдера
// серверу для этого не нужно.
//
// Почему провайдеров три:
//   - Telegram — исторически первый и самый привычный нашей аудитории вход;
//     работает и там, где нет сервисов Google.
//   - Apple — обязателен: App Store отверг сборку по guideline 4.2.3(i)
//     (вход требовал установки Telegram), а 4.8 требует вход с минимумом
//     собираемых данных. SIWA закрывает оба пункта и не требует ничего, кроме
//     самого устройства.
//   - Google — то же самое для Android, где Apple-кнопки нет.
//
// Выпилить любого из них — это убрать кнопку в клиенте и запись в конфиге:
// дальше сервера провайдер не проникает, аккаунт живёт под внутренним id
// (см. identities в store.go).
const (
	tgIssuer  = "https://oauth.telegram.org"
	tgJWKSURL = "https://oauth.telegram.org/.well-known/jwks.json"

	appleIssuer  = "https://appleid.apple.com"
	appleJWKSURL = "https://appleid.apple.com/auth/keys"

	googleJWKSURL = "https://www.googleapis.com/oauth2/v3/certs"
)

// maxNameLen — сколько рун имени принимаем. У Apple имя приходит не в токене, а
// от клиента (см. Verifier.Verify), то есть управляется устройством, поэтому
// длину ограничиваем.
const maxNameLen = 64

// Verifier — проверка ID-token одного провайдера.
//
// issuers/audiences — списки, а не строки, потому что у Google исторически два
// написания issuer, а audience зависит от платформы (свой OAuth client id на
// Android и на iOS). Из-за этого проверять их приходится вручную, а не опциями
// jwt.WithIssuer/WithAudience — те умеют только одно значение.
type Verifier struct {
	provider  string
	issuers   []string
	audiences []string
	jwksURL   string

	mu sync.Mutex
	kf keyfunc.Keyfunc // ленивая инициализация — см. keys()
}

func NewTelegramVerifier(clientID, jwksURL string) *Verifier {
	return &Verifier{
		provider:  ProviderTelegram,
		issuers:   []string{tgIssuer},
		audiences: []string{clientID},
		jwksURL:   jwksURL,
	}
}

// NewAppleVerifier: audience — bundle id приложения (net.nous.ether). Для
// нативного SIWA этого достаточно; веб-флоу (Service ID) мы не используем.
func NewAppleVerifier(bundleIDs []string) *Verifier {
	return &Verifier{
		provider:  ProviderApple,
		issuers:   []string{appleIssuer},
		audiences: bundleIDs,
		jwksURL:   appleJWKSURL,
	}
}

// NewGoogleVerifier: audience — OAuth client id, под который выписан токен. Их
// обычно несколько (Android, iOS, Web как serverClientId), поэтому список.
func NewGoogleVerifier(clientIDs []string) *Verifier {
	return &Verifier{
		provider:  ProviderGoogle,
		issuers:   []string{"https://accounts.google.com", "accounts.google.com"},
		audiences: clientIDs,
		jwksURL:   googleJWKSURL,
	}
}

// keys лениво поднимает keyfunc: первый вызов тянет JWKS и запускает фоновое
// обновление ключей. Успех кэшируем, ошибку — нет, поэтому недоступность
// провайдера на старте не валит сервер (падает только конкретный вход, а
// следующий попробует снова).
func (v *Verifier) keys() (keyfunc.Keyfunc, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.kf != nil {
		return v.kf, nil
	}
	kf, err := keyfunc.NewDefault([]string{v.jwksURL})
	if err != nil {
		return nil, err
	}
	v.kf = kf
	return kf, nil
}

// oidcClaims — объединение клеймов всех трёх провайдеров: они не конфликтуют, а
// разбирать одним типом проще, чем плодить три почти одинаковых.
type oidcClaims struct {
	jwt.RegisteredClaims
	// ID — настоящий Telegram user id. У Telegram берём его, а НЕ sub: sub там
	// бывает opaque/pairwise и вылезает за int64 (было: overflow → "bad
	// subject"). Приходит числом или строкой-числом — ловим сырьём, кавычки
	// снимаем. У Apple/Google этого клейма нет, там идентификатор — sub.
	ID                json.RawMessage `json:"id"`
	PreferredUsername string          `json:"preferred_username"` // @username (Telegram)
	Name              string          `json:"name"`               // полное отображаемое имя (Telegram, Google)
	GivenName         string          `json:"given_name"`         // имя (fallback)
	Picture           string          `json:"picture"`            // URL фото профиля (Telegram, Google)
}

// Verify проверяет ID-token (подпись по JWKS + iss/aud/exp/алгоритм) и
// возвращает пользователя провайдера.
//
// fallbackName — имя, присланное клиентом рядом с токеном. Нужно ровно для
// Apple: SIWA отдаёт имя ОДИН раз, при первой авторизации, и не в токене, а в
// ответе системного диалога, поэтому передать его может только клиент. Значение
// подконтрольно устройству, но это всего лишь отображаемое имя (его и так
// выбирает человек), поэтому ограничиваемся обрезкой; используется только когда
// сам провайдер имени не дал.
func (v *Verifier) Verify(idToken, fallbackName string) (*ProviderUser, error) {
	kf, err := v.keys()
	if err != nil {
		return nil, fmt.Errorf("jwks: %w", err)
	}
	var claims oidcClaims
	if _, err := jwt.ParseWithClaims(idToken, &claims, kf.Keyfunc,
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithExpirationRequired(),
	); err != nil {
		return nil, err
	}
	if !contains(v.issuers, claims.Issuer) {
		return nil, fmt.Errorf("issuer %q не наш", claims.Issuer)
	}
	if !anyAudience(v.audiences, claims.Audience) {
		return nil, fmt.Errorf("audience %v не наша", claims.Audience)
	}

	uid, err := v.uid(&claims)
	if err != nil {
		return nil, err
	}
	// отображаемое имя: что дал провайдер, иначе @username, иначе то, что
	// прислал клиент (путь Apple)
	name := claims.Name
	if name == "" {
		name = claims.GivenName
	}
	if name == "" {
		name = claims.PreferredUsername
	}
	if name == "" {
		name = cleanName(fallbackName)
	}
	return &ProviderUser{
		Provider:  v.provider,
		UID:       uid,
		Username:  claims.PreferredUsername,
		Name:      name,
		AvatarURL: claims.Picture,
	}, nil
}

// uid — идентификатор пользователя у провайдера: у Telegram клейм id, у
// остальных sub. Пустой не пропускаем: без него аккаунт не к чему привязать.
func (v *Verifier) uid(claims *oidcClaims) (string, error) {
	if v.provider == ProviderTelegram {
		idStr := strings.Trim(string(claims.ID), `"`)
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil || id == 0 {
			return "", fmt.Errorf("bad id %q", idStr)
		}
		return strconv.FormatInt(id, 10), nil
	}
	if claims.Subject == "" {
		return "", fmt.Errorf("пустой sub")
	}
	return claims.Subject, nil
}

// cleanName приводит присланное клиентом имя к пригодному для показа виду:
// без переводов строк (иначе поедет вёрстка ленты) и не длиннее maxNameLen.
func cleanName(s string) string {
	s = strings.TrimSpace(strings.NewReplacer("\n", " ", "\r", " ", "\t", " ").Replace(s))
	if r := []rune(s); len(r) > maxNameLen {
		s = strings.TrimSpace(string(r[:maxNameLen]))
	}
	return s
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// anyAudience — хватает совпадения по одному значению: в aud может лежать список.
func anyAudience(want []string, got jwt.ClaimStrings) bool {
	for _, a := range got {
		if contains(want, a) {
			return true
		}
	}
	return false
}
