package main

import (
	"flag"
	"log"
	"net/http"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	// прототип: пускаем любой origin
	CheckOrigin: func(r *http.Request) bool { return true },
}

// version проставляется при сборке (-ldflags "-X main.version=..."), см.
// scripts/deploy.sh; при обычном go build/run остаётся "dev".
var version = "dev"

func main() {
	env := flag.String("env", "dev", "окружение: берётся конфиг config.<env>.json")
	configPath := flag.String("config", "", "явный путь к конфигу (перекрывает -env)")
	flag.Parse()

	path := *configPath
	if path == "" {
		path = "config." + *env + ".json"
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	hub := NewHub()
	go hub.Run()

	dbPath := cfg.DB
	if dbPath == "" {
		dbPath = "ether." + *env + ".db"
	}
	store, err := OpenStore(dbPath)
	if err != nil {
		log.Fatalf("store: %v", err)
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

	mux := http.NewServeMux()
	registerREST(mux, store, tg)
	mux.HandleFunc("/ws", wsHandler(hub, geo, store))

	log.Printf("ether-server %s (%s) listening on %s (ws /ws; REST /auth/telegram /session/resume /session/logout /rules/accept /history)", version, path, cfg.Addr)
	log.Fatal(http.ListenAndServe(cfg.Addr, mux))
}

// wsHandler — апгрейд до WebSocket. ?token= опционален (можно смотреть каналы
// и читать без входа), но если прислан — должен быть валиден: клиент получает
// его из REST /auth/telegram (вход через Login Widget) или /session/resume, так
// что протухший токен здесь — сигнал рассинхронизации, а не штатный путь,
// поэтому отвечаем 401 до апгрейда. ?token= — единственный способ авторизовать
// сокет: логин-кадров на WS больше нет.
func wsHandler(hub *Hub, geo Geocoder, store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var authedUser *User
		if token := r.URL.Query().Get("token"); token != "" {
			u, err := store.UserBySession(token)
			if err != nil {
				log.Printf("ws auth: %v", err)
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
			log.Println("upgrade:", err)
			return
		}
		c := &Client{
			hub:   hub,
			conn:  conn,
			send:  make(chan Envelope, 16),
			geo:   geo,
			store: store,
		}
		if authedUser != nil {
			c.setAuthed(authedUser.TgID, authedUser.Nick, authedUser.AvatarURL)
		}
		go c.writePump()
		go c.readPump()
	}
}
