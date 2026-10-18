package main

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// SetUserChannels говорит, где участие человека в счётчике сдвинулось: по этому
// списку locate рассылает новые числа. Повторный locate на месте не должен
// давать ничего, иначе каждый запуск приложения будил бы весь канал.
func TestSetUserChannelsChanged(t *testing.T) {
	s := openTestStore(t)
	u := mkTgUser(t, s, "1", "", "Аня")
	sorted := func(ids []string) []string { slices.Sort(ids); return ids }

	got, err := s.SetUserChannels(u, []string{"EARTH", "RU", "relation/1"})
	if err != nil {
		t.Fatalf("первый locate: %v", err)
	}
	if want := []string{"EARTH", "RU", "relation/1"}; !slices.Equal(sorted(got), want) {
		t.Fatalf("первый locate: changed = %v, want %v", got, want)
	}

	if got, _ := s.SetUserChannels(u, []string{"EARTH", "RU", "relation/1"}); len(got) != 0 {
		t.Fatalf("тот же набор: changed = %v, want пусто", got)
	}

	// переезд: из района ушёл, в новый вошёл, общие каналы не тронуты
	got, _ = s.SetUserChannels(u, []string{"EARTH", "RU", "relation/2"})
	if want := []string{"relation/1", "relation/2"}; !slices.Equal(sorted(got), want) {
		t.Fatalf("переезд: changed = %v, want %v", got, want)
	}

	// вернулся после окна: в счётчике его не было — теперь есть, во всех каналах
	old := time.Now().Add(-subscriberWindow - time.Hour).UnixMilli()
	if _, err := s.db.Exec(`UPDATE user_channels SET updated_at = ? WHERE user_id = ?`, old, u); err != nil {
		t.Fatalf("состарить: %v", err)
	}
	got, _ = s.SetUserChannels(u, []string{"EARTH", "RU", "relation/2"})
	if want := []string{"EARTH", "RU", "relation/2"}; !slices.Equal(sorted(got), want) {
		t.Fatalf("возврат после окна: changed = %v, want %v", got, want)
	}

	// ушёл из канала, где его и так не считали (строка старше окна): числа это
	// не сдвигает
	if _, err := s.db.Exec(`UPDATE user_channels SET updated_at = ? WHERE user_id = ? AND channel = 'relation/2'`, old, u); err != nil {
		t.Fatalf("состарить район: %v", err)
	}
	got, _ = s.SetUserChannels(u, []string{"EARTH", "RU"})
	if len(got) != 0 {
		t.Fatalf("ушёл из канала вне окна: changed = %v, want пусто", got)
	}
}

// Сквозь сокет: когда в канал входит новый человек, у уже сидящих в нём число
// участников обновляется кадром subscribers. Сам вошедший кадров не получает —
// свежие числа у него в located.
func TestSubscribersFrameOnLocate(t *testing.T) {
	srv, store := newTestServer(t)
	wsBase := "ws" + strings.TrimPrefix(srv.URL, "http")

	dial := func(tgUID string) *websocket.Conn {
		t.Helper()
		uid := mkTgUser(t, store, tgUID, "", "u"+tgUID)
		token, err := store.NewSession(uid)
		if err != nil {
			t.Fatalf("сессия: %v", err)
		}
		ws, _, err := websocket.DefaultDialer.Dial(wsBase+fmt.Sprintf("/ws?token=%s", token), nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		t.Cleanup(func() { ws.Close() })
		return ws
	}
	locate := func(ws *websocket.Conn) {
		t.Helper()
		if err := ws.WriteJSON(envelope(TypeLocate, LocateData{Lat: 55.76, Lng: 37.61})); err != nil {
			t.Fatalf("locate: %v", err)
		}
		ws.SetReadDeadline(time.Now().Add(5 * time.Second))
		var env Envelope
		if err := ws.ReadJSON(&env); err != nil || env.Type != TypeLocated {
			t.Fatalf("ждали located: %s %v", env.Type, err)
		}
	}
	// silent — за короткое окно кадров не пришло. Только последним чтением:
	// таймаут чтения у gorilla/websocket необратим, соединение после него мертво.
	silent := func(ws *websocket.Conn, who string) {
		t.Helper()
		ws.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		var env Envelope
		if err := ws.ReadJSON(&env); err == nil {
			t.Fatalf("%s: лишний кадр %s %s", who, env.Type, env.Data)
		}
	}

	first := dial("1")
	locate(first)

	second := dial("2")
	locate(second)

	// у первого — новые числа по всем каналам точки (StubGeocoder: пять каналов)
	got := map[string]int{}
	first.SetReadDeadline(time.Now().Add(5 * time.Second))
	for len(got) < 5 {
		var env Envelope
		if err := first.ReadJSON(&env); err != nil {
			t.Fatalf("первый ждал subscribers, получил %v после %v", err, got)
		}
		if env.Type != TypeSubscribers {
			t.Fatalf("первый: кадр %s, want subscribers", env.Type)
		}
		var d SubscribersData
		mustUnmarshal(t, env.Data, &d)
		got[d.Channel] = d.Subscribers
	}
	for ch, n := range got {
		if n != 2 {
			t.Errorf("канал %s: subscribers = %d, want 2", ch, n)
		}
	}

	// повторный locate на месте ничего не сдвигает — кадров нет ни у кого; а
	// вошедший не получал их и в первый раз
	locate(second)
	silent(first, "первый после повторного locate")
	silent(second, "вошедший")
}
