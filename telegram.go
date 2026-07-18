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
	Username  string // @username (для ссылки на профиль)
	Name      string // отображаемое имя (полное `name`, иначе given_name)
	AvatarURL string // URL фото профиля (claim `picture`), может быть пустым
}

type tgClaims struct {
	jwt.RegisteredClaims
	// id — настоящий Telegram user id. Берём его, а НЕ sub: sub бывает
	// opaque/pairwise и вылезает за int64 (было: overflow → "bad subject").
	// Приходит числом или строкой-числом — ловим сырьём, кавычки снимаем.
	ID                json.RawMessage `json:"id"`
	PreferredUsername string          `json:"preferred_username"`
	Name              string          `json:"name"`       // полное отображаемое имя
	GivenName         string          `json:"given_name"` // имя (fallback, если нет name)
	Picture           string          `json:"picture"`    // URL фото профиля
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
	idStr := strings.Trim(string(claims.ID), `"`)
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id == 0 {
		return nil, fmt.Errorf("bad id %q", idStr)
	}
	// отображаемое имя: полное `name`, иначе `given_name`, иначе @username —
	// чтобы поле не осталось пустым, если Telegram не прислал имя
	name := claims.Name
	if name == "" {
		name = claims.GivenName
	}
	if name == "" {
		name = claims.PreferredUsername
	}
	return &TelegramUser{
		ID:        id,
		Username:  claims.PreferredUsername,
		Name:      name,
		AvatarURL: claims.Picture,
	}, nil
}
