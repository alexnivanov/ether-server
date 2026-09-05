package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Сокет для теста keepalive с периодами в миллисекундах вместо минут. Ручка
// своя, а не wsHandler: только она и отличается — периоды задаются соединению
// до старта помп. Возвращать глобальные значения после теста было бы гонкой:
// httptest не ждёт hijacked-соединений, и readPump переживает конец теста.
func dialKeepalive(t *testing.T, ping, pong time.Duration) *websocket.Conn {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "ka.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	hub := NewHub()
	go hub.Run()
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		c := newClient(hub, conn, StubGeocoder{}, store)
		c.pingPeriod, c.pongWait = ping, pong
		go c.writePump()
		go c.readPump()
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	ws, _, err := websocket.DefaultDialer.Dial(
		"ws"+strings.TrimPrefix(srv.URL, "http")+"/ws", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { ws.Close() })
	return ws
}

// Сервер пингует молчащее соединение сам. Это половина keepalive, которая
// держит канал живым: без трафика оператор/NAT роняет простаивающий TCP, и обе
// стороны продолжают считать сокет рабочим (см. константы в client.go).
func TestServerPingsIdleClient(t *testing.T) {
	ws := dialKeepalive(t, 30*time.Millisecond, 5*time.Second)

	pinged := make(chan struct{}, 1)
	ws.SetPingHandler(func(data string) error {
		select {
		case pinged <- struct{}{}:
		default:
		}
		return ws.WriteControl(websocket.PongMessage, []byte(data),
			time.Now().Add(time.Second))
	})
	// Управляющие кадры разбираются только при чтении — читаем в фоне.
	go func() {
		for {
			if _, _, err := ws.ReadMessage(); err != nil {
				return
			}
		}
	}()

	select {
	case <-pinged:
	case <-time.After(2 * time.Second):
		t.Fatal("сервер не прислал ping молчащему клиенту")
	}
}

// Молчащий в ответ клиент отцепляется по дедлайну чтения. Это вторая половина
// keepalive: соединение, которое уже никто не читает, иначе осталось бы в
// подписках хаба, и рассылка уходила бы в никуда.
func TestServerDropsClientWithoutPong(t *testing.T) {
	// пинг далеко за горизонтом теста: проверяем именно дедлайн чтения, а не
	// реакцию на неотвеченный ping
	ws := dialKeepalive(t, time.Hour, 100*time.Millisecond)

	// Своего дедлайна на чтении НЕТ намеренно: с ним тест проходил бы и без
	// keepalive — ошибку дало бы само истечение клиентского дедлайна, а не
	// сервер. Ждём именно того, что чтение оборвётся раньше нашего терпения.
	closed := make(chan error, 1)
	go func() {
		_, _, err := ws.ReadMessage()
		closed <- err
	}()

	select {
	case err := <-closed:
		if err == nil {
			t.Fatal("чтение закончилось без ошибки — соединение не закрыто")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("сервер не закрыл соединение после тишины")
	}
}
