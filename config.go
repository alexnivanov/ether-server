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
	// Провайдеры входа (см. oidc.go). Каждый опционален по отдельности, но хотя
	// бы один обязателен — иначе войти нечем. Подписи ID-token'ов сервер
	// проверяет по публичным ключам провайдеров, никаких их секретов не нужно.
	//
	// TelegramClientID — числовой client id приложения из @BotFather.
	// AppleClientIDs — bundle id приложения (нативный SIWA; обычно один).
	// GoogleClientIDs — OAuth client id, под которые выписан токен: у Android,
	// iOS и Web они разные, поэтому список.
	TelegramClientID string   `json:"telegram_client_id"`
	AppleClientIDs   []string `json:"apple_client_ids"`
	GoogleClientIDs  []string `json:"google_client_ids"`
	// Базовый URL Nominatim; пусто → публичный nominatim.openstreetmap.org
	// (лимит 1 req/s, не для production — в prod сюда пойдёт свой инстанс).
	NominatimURL string `json:"nominatim_url"`
	// Путь к файлу SQLite (пользователи, сессии); пусто → "ether.<env>.db".
	DB string `json:"db"`
	// FCM-пуши о новых сообщениях (опционально). Оба пусты → пуши выключены,
	// сервер работает как раньше. FCMProjectID — id проекта Firebase;
	// FCMCredentialsFile — путь к service-account JSON (в git не идёт).
	FCMProjectID       string `json:"fcm_project_id"`
	FCMCredentialsFile string `json:"fcm_credentials_file"`
	// ВНИМАНИЕ: getUpdates у бота одиночный — нельзя держать локальный сервер с
	// теми же telegram_notify_* при работающем проде: два опросчика начнут
	// отбирать друг у друга обновления (Telegram отвечает 409 Conflict). Для
	// локальной отладки нужен свой бот и свой канал.
	//
	// Служебный Telegram-канал для жалоб на сообщения (опционально): токен бота
	// (@BotFather) и chat_id канала, где этот бот — админ. Оба пусты →
	// уведомления выключены, жалобы всё равно пишутся в БД и лог. Токен —
	// секрет, в git не идёт. Требует исходящего доступа к api.telegram.org с
	// сервера (первый прод-хост в РФ его не имел — см. историю переезда).
	TelegramNotifyToken  string `json:"telegram_notify_token"`
	TelegramNotifyChatID string `json:"telegram_notify_chat_id"`
	// Sentry для ошибок и паник (опционально; пусто → выключен). См. sentry.go —
	// там же про приватность: в события попадает внутренний id пользователя, поэтому Sentry указан в
	// privacy policy как сторонний сервис.
	SentryDSN string `json:"sentry_dsn"`
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
	if cfg.TelegramClientID == "" && len(cfg.AppleClientIDs) == 0 && len(cfg.GoogleClientIDs) == 0 {
		return nil, fmt.Errorf("%s: не задан ни один провайдер входа "+
			"(telegram_client_id / apple_client_ids / google_client_ids)", path)
	}
	if cfg.Addr == "" {
		cfg.Addr = ":8080"
	}
	return &cfg, nil
}
