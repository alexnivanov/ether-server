package main

import (
	"fmt"
	"strconv"
	"sync"

	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"
)

// TelegramAuth — проверка входа через нативный Telegram Login SDK (см.
// ether-meta/PROTOCOL.md). SDK на устройстве открывает приложение Telegram и
// отдаёт клиенту OIDC ID-token (JWT), подписанный Telegram. Сервер проверяет
// его подпись по ПУБЛИЧНЫМ ключам Telegram (JWKS) и клеймы (iss/aud/exp) —
// секретный токен бота для этого не нужен вовсе.
//
// Пришло на смену и long-poll-боту, и HMAC-виджету: нативный SDK логинит через
// приложение (один тап, без номера) и корректно переживает повторный вход.
const (
	tgIssuer  = "https://oauth.telegram.org"
	tgJWKSURL = "https://oauth.telegram.org/.well-known/jwks.json"
)

type TelegramAuth struct {
	clientID string // aud — числовой client id приложения из @BotFather
	jwksURL  string

	mu sync.Mutex
	kf keyfunc.Keyfunc // ленивая инициализация — см. keys()
}

func NewTelegramAuth(clientID, jwksURL string) *TelegramAuth {
	return &TelegramAuth{clientID: clientID, jwksURL: jwksURL}
}

// keys лениво поднимает keyfunc: первый вызов тянет JWKS и запускает фоновое
// обновление ключей. Успех кэшируем, ошибку — нет, поэтому недоступность
// Telegram на старте не валит сервер (падает только конкретный вход, а
// следующий попробует снова).
func (t *TelegramAuth) keys() (keyfunc.Keyfunc, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.kf != nil {
		return t.kf, nil
	}
	kf, err := keyfunc.NewDefault([]string{t.jwksURL})
	if err != nil {
		return nil, err
	}
	t.kf = kf
	return kf, nil
}

// TelegramUser — данные из проверенного ID-token.
type TelegramUser struct {
	ID        int64
	Username  string
	Name      string // отображаемое имя (полное `name`, иначе given_name)
	AvatarURL string // URL фото профиля (claim `picture`), может быть пустым
}

// Nick — отображаемое имя: username, иначе имя, иначе "anon".
func (u TelegramUser) Nick() string {
	if u.Username != "" {
		return u.Username
	}
	if u.Name != "" {
		return u.Name
	}
	return "anon"
}

type tgClaims struct {
	jwt.RegisteredClaims
	PreferredUsername string `json:"preferred_username"`
	Name              string `json:"name"`       // полное отображаемое имя
	GivenName         string `json:"given_name"` // имя (fallback, если нет name)
	Picture           string `json:"picture"`    // URL фото профиля
}

// Verify проверяет ID-token (подпись по JWKS + iss/aud/exp/алгоритм) и
// возвращает пользователя; sub — числовой Telegram user id.
func (t *TelegramAuth) Verify(idToken string) (*TelegramUser, error) {
	kf, err := t.keys()
	if err != nil {
		return nil, fmt.Errorf("jwks: %w", err)
	}
	var claims tgClaims
	if _, err := jwt.ParseWithClaims(idToken, &claims, kf.Keyfunc,
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithIssuer(tgIssuer),
		jwt.WithAudience(t.clientID),
		jwt.WithExpirationRequired(),
	); err != nil {
		return nil, err
	}
	id, err := strconv.ParseInt(claims.Subject, 10, 64)
	if err != nil || id == 0 {
		return nil, fmt.Errorf("bad subject %q", claims.Subject)
	}
	name := claims.Name
	if name == "" {
		name = claims.GivenName
	}
	return &TelegramUser{
		ID:        id,
		Username:  claims.PreferredUsername,
		Name:      name,
		AvatarURL: claims.Picture,
	}, nil
}
