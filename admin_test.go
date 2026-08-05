package main

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func newAdminForTest(t *testing.T) (*AdminBot, *Store) {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "admin.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	// Notifier только как держатель chat_id/токена: execute по сети не ходит
	return &AdminBot{notify: NewNotifier("token", "-100123"), store: store}, store
}

func TestAdminCommands(t *testing.T) {
	a, store := newAdminForTest(t)
	// команды модерации принимают ВНУТРЕННИЙ id (он же в посте жалобы);
	// Telegram id модератор не видит вовсе
	userID := mkTgUser(t, store, "9000042", "", "Нарушитель")
	msgID, err := store.SaveMessage("RU", userID, "гадость", time.Now().UnixMilli())
	if err != nil {
		t.Fatalf("save message: %v", err)
	}

	// обычный текст в канале — молчим
	if reply := a.execute("просто заметка про модерацию"); reply != "" {
		t.Fatalf("на обычный текст ответили: %q", reply)
	}
	// незнакомая команда — тоже молчим (канал не только для бота)
	if reply := a.execute("/stats"); reply != "" {
		t.Fatalf("на незнакомую команду ответили: %q", reply)
	}
	// help не требует аргумента
	if reply := a.execute("/help"); !strings.Contains(reply, "/ban") {
		t.Fatalf("/help = %q", reply)
	}
	// мусорный аргумент не должен доходить до БД
	if reply := a.execute("/ban абв"); !strings.Contains(reply, "Не похоже на id") {
		t.Fatalf("/ban абв = %q", reply)
	}
	if reply := a.execute("/ban"); !strings.Contains(reply, "Нужен id") {
		t.Fatalf("/ban без аргумента = %q", reply)
	}

	// удаление одного сообщения
	reply := a.execute("/del " + itoa(msgID))
	if !strings.Contains(reply, "удалено") {
		t.Fatalf("/del = %q", reply)
	}
	if msgs, err := store.History("RU", 0, 10, 0); err != nil || len(msgs) != 0 {
		t.Fatalf("сообщение не удалено: %v err=%v", msgs, err)
	}
	// повторное удаление — понятный ответ, а не ошибка
	if reply := a.execute("/del " + itoa(msgID)); !strings.Contains(reply, "нет") {
		t.Fatalf("повторный /del = %q", reply)
	}

	// /ban с причиной и с именем бота в команде (как в группах): мьют на сутки
	reply = a.execute("/ban@ether_app_bot " + itoa(userID) + " реклама в квартале")
	if !strings.Contains(reply, "сутки") {
		t.Fatalf("/ban = %q", reply)
	}
	if !strings.Contains(reply, "Нарушений всего: <b>1</b>") {
		t.Fatalf("/ban не показал счётчик нарушений: %q", reply)
	}
	banned, _, permanent, reason, _ := store.BanStatus(userID)
	if !banned || permanent {
		t.Fatalf("после /ban: banned=%v permanent=%v", banned, permanent)
	}
	if reason != "реклама в квартале" {
		t.Fatalf("причина не сохранена: %q", reason)
	}

	// повторный /ban НЕ эскалирует сам: снова мьют, только счётчик растёт
	reply = a.execute("/ban " + itoa(userID))
	if strings.Contains(reply, "навсегда") {
		t.Fatalf("повторный /ban эскалировал автоматически: %q", reply)
	}
	if !strings.Contains(reply, "Нарушений всего: <b>2</b>") {
		t.Fatalf("счётчик не вырос: %q", reply)
	}
	if _, _, permanent, _, _ := store.BanStatus(userID); permanent {
		t.Fatal("повторный /ban поставил постоянный бан — эскалации быть не должно")
	}

	// снятие наказания
	if reply := a.execute("/unban " + itoa(userID)); !strings.Contains(reply, "снято") {
		t.Fatalf("/unban = %q", reply)
	}

	// постоянный бан — только явной командой
	reply = a.execute("/block " + itoa(userID) + " спам")
	if !strings.Contains(reply, "навсегда") {
		t.Fatalf("/block = %q", reply)
	}
	var users int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM users WHERE id = ?`, userID).Scan(&users); err != nil {
		t.Fatalf("count: %v", err)
	}
	if users != 0 {
		t.Fatal("аккаунт не удалён при /block")
	}
	// команда по удалённому аккаунту отвечает по-человечески, а не «смотри логи»
	if reply := a.execute("/ban " + itoa(userID)); !strings.Contains(reply, "уже удалён") {
		t.Fatalf("/ban удалённого = %q", reply)
	}
}

func TestAdminPurge(t *testing.T) {
	a, store := newAdminForTest(t)
	userID := mkTgUser(t, store, "7", "", "Спамер")
	for i := 0; i < 3; i++ {
		if _, err := store.SaveMessage("RU", userID, "спам", time.Now().UnixMilli()); err != nil {
			t.Fatalf("save: %v", err)
		}
	}
	if reply := a.execute("/purge " + itoa(userID)); !strings.Contains(reply, ": 3") {
		t.Fatalf("/purge = %q (ожидали 3 удалённых)", reply)
	}
	if msgs, err := store.History("RU", 0, 10, 0); err != nil || len(msgs) != 0 {
		t.Fatalf("после purge осталось: %v err=%v", msgs, err)
	}
}

// Команды принимаются только из служебного канала — проверяем сравнение chat_id
// (в Run это единственная авторизация).
func TestAdminChatIDGuard(t *testing.T) {
	a, _ := newAdminForTest(t)
	if a.notify.chatID != "-100123" {
		t.Fatalf("chatID = %q", a.notify.chatID)
	}
	for _, chatID := range []int64{-100123, 999} {
		match := itoa(chatID) == a.notify.chatID
		want := chatID == -100123
		if match != want {
			t.Fatalf("chat %d: match=%v, want %v", chatID, match, want)
		}
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// Модерация обязана убирать контент из ОТКРЫТЫХ лент, а не только из БД: клиенты
// историю не перезапрашивают, и без кадра `removed` удалённое сообщение висело
// бы у них до перезапуска приложения (так и было замечено на проде).
func TestAdminAnnouncesRemoval(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	a, store := newAdminForTest(t)
	a.hub = hub

	userID := mkTgUser(t, store, "77", "", "Спамер")
	msgID, err := store.SaveMessage("RU", userID, "спам", time.Now().UnixMilli())
	if err != nil {
		t.Fatalf("save: %v", err)
	}

	// подписчик канала — он и должен получить кадры
	client := &Client{send: make(chan Envelope, 8)}
	hub.subscribe <- subscription{client: client, channels: []string{"RU"}}

	next := func(what string) RemovedData {
		select {
		case env := <-client.send:
			if env.Type != TypeRemoved {
				t.Fatalf("%s: тип кадра %q, want %q", what, env.Type, TypeRemoved)
			}
			var d RemovedData
			mustUnmarshal(t, env.Data, &d)
			return d
		case <-time.After(2 * time.Second):
			t.Fatalf("%s: кадр removed не пришёл — контент останется в ленте до перезапуска", what)
			return RemovedData{}
		}
	}

	a.execute("/del " + itoa(msgID))
	if d := next("/del"); d.MessageID != msgID {
		t.Fatalf("/del: removed = %+v, want message_id %d", d, msgID)
	}

	a.execute("/purge " + itoa(userID))
	if d := next("/purge"); d.UserID != userID {
		t.Fatalf("/purge: removed = %+v, want user_id %d", d, userID)
	}

	// постоянный бан удаляет аккаунт, а с ним каскадом сообщения — про это
	// клиентам тоже надо сказать
	a.execute("/block " + itoa(userID))
	if d := next("/block"); d.UserID != userID {
		t.Fatalf("/block: removed = %+v, want user_id %d", d, userID)
	}
}
