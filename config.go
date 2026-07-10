package main

import (
	"encoding/json"
	"fmt"
	"os"
)

// Config — конфиг сервера из JSON-файла. Файл выбирается окружением:
// `-env dev` → config.dev.json, `-env prod` → config.prod.json; явный путь —
// флагом -config. Конфиги содержат секреты и в git не идут, образец —
// config.example.json.
type Config struct {
	// Адрес HTTP/WebSocket-сервера; пусто → ":8080".
	Addr string `json:"addr"`
	// Числовой client id приложения из @BotFather — audience OIDC ID-token'а
	// нативного Telegram Login SDK. Обязателен: вход через Telegram —
	// единственный механизм идентификации. Подпись токена сервер проверяет по
	// публичным ключам Telegram (JWKS), секретный токен бота ему не нужен.
	TelegramClientID string `json:"telegram_client_id"`
	// Базовый URL Nominatim; пусто → публичный nominatim.openstreetmap.org
	// (лимит 1 req/s, не для production — в prod сюда пойдёт свой инстанс).
	NominatimURL string `json:"nominatim_url"`
	// Путь к файлу SQLite (пользователи, сессии); пусто → "ether.<env>.db".
	DB string `json:"db"`
}

func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if cfg.TelegramClientID == "" {
		return nil, fmt.Errorf("%s: telegram_client_id не задан", path)
	}
	if cfg.Addr == "" {
		cfg.Addr = ":8080"
	}
	return &cfg, nil
}
