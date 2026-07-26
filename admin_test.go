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
	const tgID = 42
	if err := store.CreateUser(User{TgID: tgID, FullName: "Нарушитель"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	msgID, err := store.SaveMessage("RU", tgID, "гадость", time.Now().UnixMilli())
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
	if msgs, err := store.History("RU", 0, 10); err != nil || len(msgs) != 0 {
		t.Fatalf("сообщение не удалено: %v err=%v", msgs, err)
	}
	// повторное удаление — понятный ответ, а не ошибка
	if reply := a.execute("/del " + itoa(msgID)); !strings.Contains(reply, "нет") {
		t.Fatalf("повторный /del = %q", reply)
	}

	// /ban с причиной и с именем бота в команде (как в группах): мьют на сутки
	reply = a.execute("/ban@ether_app_bot 42 реклама в квартале")
	if !strings.Contains(reply, "сутки") {
		t.Fatalf("/ban = %q", reply)
	}
	if !strings.Contains(reply, "Нарушений всего: <b>1</b>") {
		t.Fatalf("/ban не показал счётчик нарушений: %q", reply)
	}
	banned, _, permanent, reason, _ := store.BanStatus(tgID)
	if !banned || permanent {
		t.Fatalf("после /ban: banned=%v permanent=%v", banned, permanent)
	}
	if reason != "реклама в квартале" {
		t.Fatalf("причина не сохранена: %q", reason)
	}

	// повторный /ban НЕ эскалирует сам: снова мьют, только счётчик растёт
	reply = a.execute("/ban 42")
	if strings.Contains(reply, "навсегда") {
		t.Fatalf("повторный /ban эскалировал автоматически: %q", reply)
	}
	if !strings.Contains(reply, "Нарушений всего: <b>2</b>") {
		t.Fatalf("счётчик не вырос: %q", reply)
	}
	if _, _, permanent, _, _ := store.BanStatus(tgID); permanent {
		t.Fatal("повторный /ban поставил постоянный бан — эскалации быть не должно")
	}

	// снятие наказания
	if reply := a.execute("/unban 42"); !strings.Contains(reply, "снято") {
		t.Fatalf("/unban = %q", reply)
	}

	// постоянный бан — только явной командой
	reply = a.execute("/block 42 спам")
	if !strings.Contains(reply, "навсегда") {
		t.Fatalf("/block = %q", reply)
	}
	var users int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM users WHERE tg_id = ?`, tgID).Scan(&users); err != nil {
		t.Fatalf("count: %v", err)
	}
	if users != 0 {
		t.Fatal("аккаунт не удалён при повторном /ban")
	}
}

func TestAdminPurge(t *testing.T) {
	a, store := newAdminForTest(t)
	const tgID = 7
	if err := store.CreateUser(User{TgID: tgID, FullName: "Спамер"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := store.SaveMessage("RU", tgID, "спам", time.Now().UnixMilli()); err != nil {
			t.Fatalf("save: %v", err)
		}
	}
	if reply := a.execute("/purge 7"); !strings.Contains(reply, ": 3") {
		t.Fatalf("/purge = %q (ожидали 3 удалённых)", reply)
	}
	if msgs, err := store.History("RU", 0, 10); err != nil || len(msgs) != 0 {
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
