package main

import (
	"testing"
	"time"
)

// Кадр «всем подключённым» приходит клиенту один раз, сколько бы каналов у него
// ни было.
func TestAnnounceOncePerClient(t *testing.T) {
	hub := NewHub()
	go hub.Run()

	c := &Client{send: make(chan Envelope, 8)}
	hub.subscribe <- subscription{client: c, channels: []string{"EARTH", "RU", "RU-MOW"}}
	hub.AnnounceRemoved(RemovedData{MessageID: 1})

	select {
	case <-c.send:
	case <-time.After(2 * time.Second):
		t.Fatal("кадр removed не пришёл")
	}
	select {
	case env := <-c.send:
		t.Fatalf("кадр пришёл повторно: %+v", env)
	case <-time.After(100 * time.Millisecond):
	}
}

// Повторный locate в том же соединении заменяет набор: из покинутого канала
// в сокет больше ничего не приходит, из оставшегося и нового — приходит.
func TestSubscribeReplacesChannels(t *testing.T) {
	hub := NewHub()
	go hub.Run()

	c := &Client{send: make(chan Envelope, 8)}
	hub.subscribe <- subscription{client: c, channels: []string{"A", "B"}}
	hub.subscribe <- subscription{client: c, channels: []string{"A", "C"}}

	hub.broadcast <- MessageData{ID: 1, Channel: "B"}
	hub.broadcast <- MessageData{ID: 2, Channel: "A"}
	hub.broadcast <- MessageData{ID: 3, Channel: "C"}

	var got []int64
	for len(got) < 2 {
		select {
		case env := <-c.send:
			var m MessageData
			mustUnmarshal(t, env.Data, &m)
			got = append(got, m.ID)
		case <-time.After(2 * time.Second):
			t.Fatalf("пришли %v, want [2 3]", got)
		}
	}
	if got[0] != 2 || got[1] != 3 {
		t.Fatalf("пришли %v, want [2 3]: покинутый канал B всё ещё шлёт в сокет", got)
	}
	select {
	case env := <-c.send:
		t.Fatalf("лишний кадр: %+v", env)
	case <-time.After(100 * time.Millisecond):
	}

	// после переподписки отключение по-прежнему закрывает send и убирает
	// клиента изо всех каналов
	hub.unregister <- c
	hub.broadcast <- MessageData{ID: 4, Channel: "A"}
	if _, ok := <-c.send; ok {
		t.Fatal("после unregister send не закрыт")
	}
	// Хаб к этому моменту ждёт в select (закрытие send — последнее, что он
	// сделал), так что читать его карты здесь безопасно. Ни пустых каналов, ни
	// записи в обратном индексе остаться не должно, иначе они копятся с
	// каждым соединением.
	if len(hub.channels) != 0 || len(hub.joined) != 0 {
		t.Fatalf("после unregister остались channels=%v joined=%v", hub.channels, hub.joined)
	}
}
