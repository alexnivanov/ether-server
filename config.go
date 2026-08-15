package main

import (
	"fmt"
	"os"

	"github.com/goccy/go-yaml"
)

// Config — конфиг сервера из YAML-файла. Файл выбирается окружением:
// `-env dev` → config.dev.yaml, `-env prod` → config.prod.yaml; явный путь —
// флагом -config. Конфиги содержат секреты и в git не идут, образец —
// config.example.yaml.
//
// YAML, а не JSON: конфиг правят руками на сервере, а там нужны комментарии
// (в JSON их пришлось бы подделывать плейсхолдерами) и прощающий синтаксис
// без запятых и кавычек.
type Config struct {
	// Адрес HTTP/WebSocket-сервера; пусто → ":8080".
	Addr string `yaml:"addr"`
	// Провайдеры входа (см. oidc.go). Каждый опционален по отдельности, но хотя
	// бы один обязателен — иначе войти нечем. Подписи ID-token'ов сервер
	// проверяет по публичным ключам провайдеров, никаких их секретов не нужно.
	//
	// TelegramClientID — числовой client id приложения из @BotFather.
	// AppleClientIDs — bundle id приложения (нативный SIWA; обычно один).
	// GoogleClientIDs — OAuth client id, под которые выписан токен: у Android,
	// iOS и Web они разные, поэтому список.
	TelegramClientID string   `yaml:"telegram_client_id"`
	AppleClientIDs   []string `yaml:"apple_client_ids"`
	GoogleClientIDs  []string `yaml:"google_client_ids"`
	// Базовый URL Nominatim; пусто → публичный nominatim.openstreetmap.org
	// (лимит 1 req/s, не для production — в prod сюда пойдёт свой инстанс).
	NominatimURL string `yaml:"nominatim_url"`
	// Путь к файлу SQLite (пользователи, сессии); пусто → "ether.<env>.db".
	DB string `yaml:"db"`
	// FCM-пуши о новых сообщениях (опционально). Оба пусты → пуши выключены,
	// сервер работает как раньше. FCMProjectID — id проекта Firebase;
	// FCMCredentialsFile — путь к service-account JSON (в git не идёт).
	FCMProjectID       string `yaml:"fcm_project_id"`
	FCMCredentialsFile string `yaml:"fcm_credentials_file"`
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
	TelegramNotifyToken  string `yaml:"telegram_notify_token"`
	TelegramNotifyChatID string `yaml:"telegram_notify_chat_id"`
	// Sentry для ошибок и паник (опционально; пусто → выключен). См. sentry.go —
	// там же про приватность: в события попадает внутренний id пользователя, поэтому Sentry указан в
	// privacy policy как сторонний сервис.
	SentryDSN string `yaml:"sentry_dsn"`
}

func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg, err := parseConfig(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// parseConfig разбирает YAML и проставляет умолчания. Строгий режим: опечатка в
// ключе (`telegram_notify_chatid`) — ошибка старта, а не молча пустое поле.
func parseConfig(b []byte) (*Config, error) {
	var cfg Config
	if err := yaml.UnmarshalWithOptions(b, &cfg, yaml.Strict()); err != nil {
		return nil, err
	}
	if cfg.TelegramClientID == "" && len(cfg.AppleClientIDs) == 0 && len(cfg.GoogleClientIDs) == 0 {
		return nil, fmt.Errorf("не задан ни один провайдер входа " +
			"(telegram_client_id / apple_client_ids / google_client_ids)")
	}
	if cfg.Addr == "" {
		cfg.Addr = ":8080"
	}
	return &cfg, nil
}
