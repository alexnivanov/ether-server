package main

import (
	"flag"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"

	sentryhttp "github.com/getsentry/sentry-go/http"
	"github.com/gorilla/websocket"
)

// setupLogging ставит slog глобальным логгером с компактным человекочитаемым
// форматом (см. consoleHandler в logging.go): "LEVEL [component] message key=val",
// без времени — под systemd его добавляет journald. Уровень — Info (Debug-вызовов
// пока нет). Пишем в stderr, journald его подхватывает.
func setupLogging() {
	slog.SetDefault(slog.New(newConsoleHandler(os.Stderr, slog.LevelInfo)))
}

var upgrader = websocket.Upgrader{
	// прототип: пускаем любой origin
	CheckOrigin: func(r *http.Request) bool { return true },
}

// version проставляется при сборке (-ldflags "-X main.version=..."), см.
// scripts/deploy.sh; при обычном go build/run остаётся "dev".
var version = "dev"

func main() {
	setupLogging()

	env := flag.String("env", "dev", "окружение: берётся конфиг config.<env>.yaml")
	configPath := flag.String("config", "", "явный путь к конфигу (перекрывает -env)")
	flag.Parse()

	path := *configPath
	if path == "" {
		path = "config." + *env + ".yaml"
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		slog.Error("config", "err", err)
		os.Exit(1)
	}

	// Sentry: ошибки и паники. Оборачиваем логгер, чтобы записи уровня ERROR
	// уходили туда же, куда пишутся (см. sentry.go) — без дублирующих вызовов
	// SDK по коду.
	flushSentry, err := initSentry(cfg.SentryDSN, *env, version)
	if err != nil {
		slog.Warn("sentry disabled (init error)", "err", err)
	} else if cfg.SentryDSN != "" {
		slog.SetDefault(slog.New(newSentryHandler(newConsoleHandler(os.Stderr, slog.LevelInfo))))
	}
	defer flushSentry()
	slog.Info("sentry", "enabled", cfg.SentryDSN != "")

	hub := NewHub()
	go hub.Run()

	dbPath := cfg.DB
	if dbPath == "" {
		dbPath = "ether." + *env + ".db"
	}
	store, err := OpenStore(dbPath)
	if err != nil {
		slog.Error("store", "err", err)
		os.Exit(1)
	}
	go startMessageCleanup(store) // сообщения живут messageTTL, см. cleanup.go

	nominatim := NewNominatimGeocoder()
	if cfg.NominatimURL != "" {
		nominatim.BaseURL = cfg.NominatimURL
	}
	// Кеш поверх геокодера: набор каналов для точки стабилен, а публичный
	// Nominatim ограничен 1 req/s (см. geocache.go). Без него задержка упирается
	// в потолок уже на десятках активных пользователей.
	var geo Geocoder = newCachedGeocoder(nominatim, geocodeCacheTTL, geocodeCacheMax)

	// Провайдеры входа: сервер проверяет ID-token по публичным ключам провайдера
	// (JWKS тянется лениво при первом входе), поэтому старт не зависит от их
	// доступности. Незаданный провайдер эндпоинта не получает — см. registerREST.
	verifiers := map[string]*Verifier{}
	if cfg.TelegramClientID != "" {
		verifiers[ProviderTelegram] = NewTelegramVerifier(cfg.TelegramClientID, tgJWKSURL)
	}
	if len(cfg.AppleClientIDs) > 0 {
		verifiers[ProviderApple] = NewAppleVerifier(cfg.AppleClientIDs)
	}
	if len(cfg.GoogleClientIDs) > 0 {
		verifiers[ProviderGoogle] = NewGoogleVerifier(cfg.GoogleClientIDs)
	}
	providers := make([]string, 0, len(verifiers))
	for p := range verifiers {
		providers = append(providers, p)
	}
	sort.Strings(providers)
	slog.Info("auth providers", "enabled", strings.Join(providers, ","))

	// FCM-пуши опциональны: без creds в конфиге push == nil и publish работает
	// как раньше. Ошибка чтения creds не валит старт — просто без пушей.
	push, err := NewPusher(cfg.FCMProjectID, cfg.FCMCredentialsFile, store)
	if err != nil {
		slog.Warn("fcm disabled (creds error)", "err", err)
	}
	slog.Info("fcm", "enabled", push != nil)

	// уведомления модерации в служебный Telegram-канал: без токена/chat_id в
	// конфиге notify == nil, жалобы всё равно принимаются (БД + лог)
	notify := NewNotifier(cfg.TelegramNotifyToken, cfg.TelegramNotifyChatID)
	slog.Info("moderation notify", "enabled", notify != nil)

	// еженедельная сводка в тот же канал (суббота 7:00 UTC = 10:00 MSK) — см.
	// stats.go. Без
	// notify отправлять некуда, и горутину незачем держать.
	if notify != nil {
		go startWeeklyStats(store, notify)
	}
	slog.Info("weekly stats", "enabled", notify != nil)

	// команды модерации из того же канала (/ban, /del, ...) — см. admin.go
	if admin := NewAdminBot(notify, store, hub); admin != nil {
		go admin.Run()
		slog.Info("moderation commands", "enabled", true)
	}

	// пороги версий клиента (см. version.go). Неверный порог — ошибка старта, а
	// не молчание: он либо тихо блокирует людей, либо тихо ничего не делает, и
	// заметить это можно только по жалобам.
	gate, err := newVersionGate(cfg.ClientVersions)
	if err != nil {
		slog.Error("client versions", "err", err)
		os.Exit(1)
	}
	slog.Info("client versions", "platforms", len(cfg.ClientVersions))

	mux := http.NewServeMux()
	registerREST(mux, store, verifiers, notify, gate)
	// лимитер один на процесс: частота считается на аккаунт, а не на соединение
	mux.HandleFunc("/ws", wsHandler(hub, geo, store, push, NewRateLimiter()))

	// Паники в хендлерах: net/http гасит их внутри соединения, и мы бы о них не
	// узнали. sentryhttp перехватывает, отправляет со стектрейсом и не даёт
	// процессу упасть.
	var handler http.Handler = mux
	if cfg.SentryDSN != "" {
		handler = sentryhttp.New(sentryhttp.Options{Repanic: false}).Handle(mux)
	}

	slog.Info("listening", "version", version, "config", path, "addr", cfg.Addr)
	if err := http.ListenAndServe(cfg.Addr, handler); err != nil {
		slog.Error("listen", "err", err)
		os.Exit(1)
	}
}

// wsHandler — апгрейд до WebSocket. ?token= опционален (можно смотреть каналы
// и читать без входа), но если прислан — должен быть валиден: клиент получает
// его из REST /auth/telegram (вход через Login Widget) или /session/resume, так
// что протухший токен здесь — сигнал рассинхронизации, а не штатный путь,
// поэтому отвечаем 401 до апгрейда. ?token= — единственный способ авторизовать
// сокет: логин-кадров на WS больше нет.
func wsHandler(hub *Hub, geo Geocoder, store *Store, push *Pusher, limiter *RateLimiter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var authedUser *User
		if token := r.URL.Query().Get("token"); token != "" {
			u, err := store.UserBySession(token)
			if err != nil {
				slog.Error("ws auth", "err", err)
				http.Error(w, "session lookup failed", http.StatusInternalServerError)
				return
			}
			if u == nil {
				http.Error(w, "bad session", http.StatusUnauthorized)
				return
			}
			// Сокет не открываем только при постоянном бане (аккаунт удалён).
			// Временный бан — мьют: соединение нужно, чтобы читать; отправка
			// отбивается в publish (см. client.go).
			if u.BanPermanent {
				http.Error(w, "banned", http.StatusForbidden)
				return
			}
			authedUser = u
		}

		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			slog.Warn("ws upgrade failed", "err", err)
			return
		}
		c := &Client{
			hub:     hub,
			conn:    conn,
			send:    make(chan Envelope, 16),
			geo:     geo,
			store:   store,
			push:    push,
			limiter: limiter,
		}
		if authedUser != nil {
			c.setAuthed(
				authedUser.ID,
				authedUser.FullName,
				authedUser.TgUsername,
				authedUser.AvatarURL,
				authedUser.CreatedAt,
			)
		}
		go c.writePump()
		go c.readPump()
	}
}
