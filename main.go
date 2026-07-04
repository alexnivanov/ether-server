package main

import (
	"log"
	"net/http"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	// прототип: пускаем любой origin
	CheckOrigin: func(r *http.Request) bool { return true },
}

func main() {
	hub := NewHub()
	go hub.Run()

	var geo Geocoder = NewNominatimGeocoder() // прототип: публичный Nominatim, 1 req/s

	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Println("upgrade:", err)
			return
		}
		c := &Client{
			hub:  hub,
			conn: conn,
			send: make(chan Envelope, 16),
			geo:  geo,
			nick: "anon",
		}
		go c.writePump()
		go c.readPump()
	})

	const addr = ":8080"
	log.Printf("ether-server listening on %s (ws://localhost%s/ws)", addr, addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}
