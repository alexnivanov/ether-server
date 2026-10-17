package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Лента объявлений: целиком или новее after_id, хронологически.
func TestStoreAnnouncements(t *testing.T) {
	s := openTestStore(t)
	var ids []int64
	for i, text := range []string{"раз", "два", "три"} {
		id, err := s.SaveAnnouncement(text, int64(1000+i))
		if err != nil {
			t.Fatalf("save: %v", err)
		}
		ids = append(ids, id)
	}

	list, err := s.Announcements(0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 3 || list[0].ID != ids[0] || list[2].ID != ids[2] ||
		list[0].Text != "раз" || list[0].TS != 1000 {
		t.Fatalf("лента = %+v, want три по возрастанию", list)
	}
	newer, _ := s.Announcements(ids[0])
	if len(newer) != 2 || newer[0].ID != ids[1] || newer[1].ID != ids[2] {
		t.Fatalf("новее первого = %+v, want два по возрастанию", newer)
	}
	if none, _ := s.Announcements(ids[2]); len(none) != 0 || none == nil {
		t.Fatalf("новее последнего = %#v, want пустой массив", none)
	}
}

// Пуш объявления идёт всем устройствам, а не подписчикам канала.
func TestStoreAllDeviceTokens(t *testing.T) {
	s := openTestStore(t)
	a := mkTgUser(t, s, "1", "", "A")
	b := mkTgUser(t, s, "2", "", "B")
	for tok, uid := range map[string]int64{"t-a1": a, "t-a2": a, "t-b": b} {
		if err := s.SaveDeviceToken(uid, tok, "ios"); err != nil {
			t.Fatalf("save token: %v", err)
		}
	}
	got, err := s.AllDeviceTokens()
	if err != nil || len(got) != 3 {
		t.Fatalf("AllDeviceTokens = %v, %v; want 3 токена", got, err)
	}
}

// GET /announcements — публично, без токена, той же формы, что кадр: вся лента
// или с after_id только новее.
// Пустая лента — пустой массив, а не null: клиенту не нужно различать.
func TestAnnouncementsREST(t *testing.T) {
	srv, store := newTestServer(t)
	if body := getAnnouncements(t, srv.URL, ""); body.Announcements == nil || len(body.Announcements) != 0 {
		t.Fatalf("пустая лента = %+v, want []", body)
	}
	for _, text := range []string{"первое", "второе"} {
		if _, err := store.SaveAnnouncement(text, time.Now().UnixMilli()); err != nil {
			t.Fatalf("save: %v", err)
		}
	}
	body := getAnnouncements(t, srv.URL, "")
	if len(body.Announcements) != 2 || body.Announcements[0].Text != "первое" ||
		body.Announcements[1].Text != "второе" || body.Announcements[1].TS == 0 {
		t.Fatalf("ответ = %+v, want оба объявления по порядку, со временем", body)
	}
	first := body.Announcements[0].ID
	if got := getAnnouncements(t, srv.URL, "?after_id="+itoa(first)); len(got.Announcements) != 1 ||
		got.Announcements[0].Text != "второе" {
		t.Fatalf("after_id = %+v, want только второе", got)
	}
	// мусорный after_id — как без него, а не 400: проверка при старте не
	// должна ломаться из-за кривого значения в хранилище клиента
	if got := getAnnouncements(t, srv.URL, "?after_id=abc"); len(got.Announcements) != 2 {
		t.Fatalf("мусорный after_id = %+v, want вся лента", got)
	}
}

func getAnnouncements(t *testing.T, base, query string) AnnouncementsData {
	t.Helper()
	resp, err := http.Get(base + "/announcements" + query)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var body AnnouncementsData
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body
}

// /news в админ-боте: публикация с переносами строк как есть и кадром всем
// подключённым, внятный ответ на пустой и слишком длинный текст.
func TestAdminNews(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	a, store := newAdminForTest(t)
	a.hub = hub

	// кадры объявлений идут всем, кто прислал locate, на какой бы канал он ни
	// был подписан
	client := &Client{send: make(chan Envelope, 8)}
	hub.subscribe <- subscription{client: client, channels: []string{"relation/1"}}
	next := func(what, wantType string, v any) {
		t.Helper()
		select {
		case env := <-client.send:
			if env.Type != wantType {
				t.Fatalf("%s: тип кадра %q, want %q", what, env.Type, wantType)
			}
			mustUnmarshal(t, env.Data, v)
		case <-time.After(2 * time.Second):
			t.Fatalf("%s: кадр %s не пришёл", what, wantType)
		}
	}

	if reply := a.execute("/news"); !strings.Contains(reply, "Нужен текст") {
		t.Fatalf("/news без текста: %q", reply)
	}
	if reply := a.execute("/news " + strings.Repeat("я", maxMessageLen)); !strings.Contains(reply, "Нужен текст") {
		t.Fatalf("/news длиннее предела: %q", reply)
	}

	text := "Вышла 1.6\n\nЧто нового:\n— треды"
	reply := a.execute("/news@ether_app_bot  " + text + "  ")
	if !strings.Contains(reply, "опубликовано") || !strings.Contains(reply, "пуши выключены") {
		t.Fatalf("/news: %q", reply)
	}
	var pub AnnouncementData
	next("/news", TypeAnnouncement, &pub)
	if pub.Text != text || pub.ID == 0 || pub.TS == 0 {
		t.Fatalf("кадр announcement = %+v, want текст с переносами как есть", pub)
	}
	stored, _ := store.Announcements(0)
	if len(stored) != 1 || stored[0].Text != text {
		t.Fatalf("в базе = %+v", stored)
	}

	// правки и снятия нет: команды не наши, бот молчит
	for _, cmd := range []string{"/news_edit " + itoa(pub.ID) + " x", "/news_del " + itoa(pub.ID)} {
		if reply := a.execute(cmd); reply != "" {
			t.Fatalf("%s: ответили %q", cmd, reply)
		}
	}

	if reply := a.execute("/help"); !strings.Contains(reply, "/news") {
		t.Fatalf("/help без команд объявлений: %q", reply)
	}
}
