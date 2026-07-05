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
	// Токен бота от @BotFather. Обязателен: вход через Telegram — единственный
	// механизм идентификации, без него сервер не запускается.
	TelegramBotToken string `json:"telegram_bot_token"`
	// Базовый URL Nominatim; пусто → публичный nominatim.openstreetmap.org
	// (лимит 1 req/s, не для production — в prod сюда пойдёт свой инстанс).
	NominatimURL string `json:"nominatim_url"`
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
	if cfg.TelegramBotToken == "" {
		return nil, fmt.Errorf("%s: telegram_bot_token не задан", path)
	}
	if cfg.Addr == "" {
		cfg.Addr = ":8080"
	}
	return &cfg, nil
}
