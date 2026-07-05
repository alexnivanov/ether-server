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

	tg, err := NewTelegramAuth(cfg.TelegramBotToken, hub, store)
	if err != nil {
		log.Fatalf("telegram: %v", err)
	}

	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
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
			tg:    tg,
			store: store,
		}
		hub.register <- c
		go c.writePump()
		go c.readPump()
	})

	log.Printf("ether-server (%s) listening on %s (ws-эндпоинт /ws)", path, cfg.Addr)
	log.Fatal(http.ListenAndServe(cfg.Addr, nil))
}
