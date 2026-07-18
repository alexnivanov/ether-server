package main

import (
	"flag"
	"log/slog"
	"net/http"
	"os"

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

	env := flag.String("env", "dev", "окружение: берётся конфиг config.<env>.json")
	configPath := flag.String("config", "", "явный путь к конфигу (перекрывает -env)")
	flag.Parse()

	path := *configPath
	if path == "" {
		path = "config." + *env + ".json"
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		slog.Error("config", "err", err)
		os.Exit(1)
	}

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

	nominatim := NewNominatimGeocoder()
	if cfg.NominatimURL != "" {
		nominatim.BaseURL = cfg.NominatimURL
	}
	var geo Geocoder = nominatim

	// вход через нативный Telegram Login SDK: сервер проверяет OIDC ID-token по
	// публичным ключам Telegram (JWKS тянется лениво при первом входе), поэтому
	// старт не зависит от доступности Telegram
	tg := NewTelegramAuth(cfg.TelegramClientID, tgJWKSURL)

	// FCM-пуши опциональны: без creds в конфиге push == nil и publish работает
	// как раньше. Ошибка чтения creds не валит старт — просто без пушей.
	push, err := NewPusher(cfg.FCMProjectID, cfg.FCMCredentialsFile)
	if err != nil {
		slog.Warn("fcm disabled (creds error)", "err", err)
	}
	slog.Info("fcm", "enabled", push != nil)

	mux := http.NewServeMux()
	registerREST(mux, store, tg)
	mux.HandleFunc("/ws", wsHandler(hub, geo, store, push))

	slog.Info("listening", "version", version, "config", path, "addr", cfg.Addr)
	if err := http.ListenAndServe(cfg.Addr, mux); err != nil {
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
func wsHandler(hub *Hub, geo Geocoder, store *Store, push *Pusher) http.HandlerFunc {
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
			authedUser = u
		}

		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			slog.Warn("ws upgrade failed", "err", err)
			return
		}
		c := &Client{
			hub:   hub,
			conn:  conn,
			send:  make(chan Envelope, 16),
			geo:   geo,
			store: store,
			push:  push,
		}
		if authedUser != nil {
			c.setAuthed(
				authedUser.TgID,
				authedUser.FullName,
				authedUser.TgUsername,
				authedUser.AvatarURL,
			)
		}
		go c.writePump()
		go c.readPump()
	}
}
