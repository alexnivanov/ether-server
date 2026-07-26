package main

import (
	"path/filepath"
	"testing"
	"time"
)

func TestStoreUsersAndSessions(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	u := User{TgID: 42, TgUsername: "alex", FullName: "Alex"}
	// до создания UpdateUser никого не находит — по этому признаку вход считается
	// первым (см. handleAuthTelegram)
	if found, _, err := s.UpdateUser(u); err != nil {
		t.Fatalf("update до create: %v", err)
	} else if found {
		t.Fatal("update до create: found = true, want false")
	}
	if err := s.CreateUser(u); err != nil {
		t.Fatalf("create: %v", err)
	}

	token, err := s.NewSession(u.TgID)
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	if token == "" {
		t.Fatal("empty session token")
	}

	got, err := s.UserBySession(token)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got == nil || *got != u {
		t.Fatalf("resume: got %+v, want %+v", got, u)
	}

	// неизвестный токен — не ошибка, а «сессии нет»
	got, err = s.UserBySession("deadbeef")
	if err != nil {
		t.Fatalf("unknown token: %v", err)
	}
	if got != nil {
		t.Fatalf("unknown token: got %+v, want nil", got)
	}

	// повторный вход обновляет ник, старая сессия видит новый
	u.FullName = "alex_new"
	if found, accepted, err := s.UpdateUser(u); err != nil {
		t.Fatalf("update: %v", err)
	} else if !found {
		t.Fatal("update: found = false для существующего пользователя")
	} else if accepted {
		t.Fatal("update: rules_accepted = true до accept_rules")
	}
	got, err = s.UserBySession(token)
	if err != nil {
		t.Fatalf("resume after update: %v", err)
	}
	if got == nil || got.FullName != "alex_new" {
		t.Fatalf("resume after update: got %+v, want full_name alex_new", got)
	}

	// правила принимаются один раз и переживают повторные входы (UpdateUser)
	if err := s.AcceptRules(u.TgID); err != nil {
		t.Fatalf("accept rules: %v", err)
	}
	if _, accepted, err := s.UpdateUser(u); err != nil {
		t.Fatalf("update after accept: %v", err)
	} else if !accepted {
		t.Fatal("update after accept: rules_accepted = false, want true")
	}
	got, err = s.UserBySession(token)
	if err != nil {
		t.Fatalf("resume after accept: %v", err)
	}
	if got == nil || !got.RulesAccepted {
		t.Fatalf("resume after accept: got %+v, want RulesAccepted=true", got)
	}
}

func TestStoreMessages(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	// автор: ник/@username/аватар в messages не хранятся — History берёт их
	// JOIN из users по tg_id
	if err := s.CreateUser(User{
		TgID:       42,
		TgUsername: "alex_tg",
		FullName:   "alex",
		AvatarURL:  "https://t.me/i/alex.jpg",
	}); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	var ids []int64
	for i, text := range []string{"один", "два", "три", "четыре", "пять"} {
		id, err := s.SaveMessage("RU", 42, text, int64(1000+i))
		if err != nil {
			t.Fatalf("save %q: %v", text, err)
		}
		ids = append(ids, id)
	}
	if _, err := s.SaveMessage("DE", 42, "hallo", 2000); err != nil {
		t.Fatalf("save DE: %v", err)
	}

	// последняя страница: 3 новейших, хронологически
	msgs, err := s.History("RU", 0, 3)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(msgs) != 3 || msgs[0].Text != "три" || msgs[2].Text != "пять" {
		t.Fatalf("history page 1: %+v", msgs)
	}
	if msgs[0].ID != ids[2] || msgs[0].Channel != "RU" || msgs[0].TS != 1002 {
		t.Fatalf("history fields: %+v", msgs[0])
	}
	// ник и аватар подтянуты JOIN из users
	if msgs[0].Sender != "alex" ||
		msgs[0].SenderID != 42 ||
		msgs[0].Username != "alex_tg" ||
		msgs[0].AvatarURL != "https://t.me/i/alex.jpg" {
		t.Fatalf("history join users: %+v", msgs[0])
	}

	// страница вверх от начала предыдущей
	msgs, err = s.History("RU", msgs[0].ID, 10)
	if err != nil {
		t.Fatalf("history before: %v", err)
	}
	if len(msgs) != 2 || msgs[0].Text != "один" || msgs[1].Text != "два" {
		t.Fatalf("history page 2: %+v", msgs)
	}

	// чужой канал не подмешивается, пустой — пустой список
	msgs, err = s.History("FR", 0, 10)
	if err != nil {
		t.Fatalf("history empty: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("history empty: %+v", msgs)
	}
}

func TestStoreDeleteUser(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	u := User{TgID: 7, TgUsername: "alex", FullName: "Alex", AvatarURL: "https://t.me/i/a.jpg"}
	if err := s.CreateUser(u); err != nil {
		t.Fatalf("create: %v", err)
	}
	// два устройства — две сессии
	t1, err := s.NewSession(u.TgID)
	if err != nil {
		t.Fatalf("session 1: %v", err)
	}
	t2, err := s.NewSession(u.TgID)
	if err != nil {
		t.Fatalf("session 2: %v", err)
	}
	if _, err := s.SaveMessage("RU", u.TgID, "привет", 1000); err != nil {
		t.Fatalf("save message: %v", err)
	}

	if err := s.DeleteUser(u.TgID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// все сессии пользователя мертвы (удаление аккаунта — со всех устройств)
	for _, tok := range []string{t1, t2} {
		got, err := s.UserBySession(tok)
		if err != nil {
			t.Fatalf("resume after delete: %v", err)
		}
		if got != nil {
			t.Fatalf("resume after delete: сессия %s жива: %+v", tok, got)
		}
	}

	// сообщения удалены каскадом вместе с аккаунтом (ON DELETE CASCADE)
	msgs, err := s.History("RU", 0, 10)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("history: got %d messages, want 0 (удалены вместе с аккаунтом)", len(msgs))
	}

	// повторное удаление — не ошибка (идемпотентно)
	if err := s.DeleteUser(u.TgID); err != nil {
		t.Fatalf("delete again: %v", err)
	}
}

func TestStoreDeleteMessagesOlderThan(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	if err := s.CreateUser(User{TgID: 42, FullName: "alex"}); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	ttl := 7 * 24 * time.Hour
	now := time.Now()
	// два сообщения по краям TTL: одно чуть старше границы, одно свежее
	stale, err := s.SaveMessage("RU", 42, "старое", now.Add(-ttl-time.Hour).UnixMilli())
	if err != nil {
		t.Fatalf("save stale: %v", err)
	}
	if _, err := s.SaveMessage("RU", 42, "свежее", now.Add(-time.Hour).UnixMilli()); err != nil {
		t.Fatalf("save fresh: %v", err)
	}

	n, err := s.DeleteMessagesOlderThan(ttl)
	if err != nil {
		t.Fatalf("delete old: %v", err)
	}
	if n != 1 {
		t.Fatalf("удалено %d сообщений, want 1", n)
	}

	msgs, err := s.History("RU", 0, 10)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Text != "свежее" {
		t.Fatalf("после уборки осталось %+v, want только «свежее»", msgs)
	}
	if msgs[0].ID == stale {
		t.Fatal("осталось именно старое сообщение")
	}

	// повторный проход больше нечего удалять
	if n, err := s.DeleteMessagesOlderThan(ttl); err != nil || n != 0 {
		t.Fatalf("повторная уборка: n=%d err=%v, want 0 nil", n, err)
	}
}

func TestStoreReportMessage(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	author := User{TgID: 1, FullName: "Author"}
	reporter := User{TgID: 2, FullName: "Reporter"}
	for _, u := range []User{author, reporter} {
		if err := s.CreateUser(u); err != nil {
			t.Fatalf("create %d: %v", u.TgID, err)
		}
	}
	msgID, err := s.SaveMessage("RU", author.TgID, "гадость", 1000)
	if err != nil {
		t.Fatalf("save message: %v", err)
	}

	rep, err := s.ReportMessage(msgID, reporter.TgID, "abuse")
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if rep == nil {
		t.Fatal("report: nil для существующего сообщения")
	}
	if !rep.Fresh {
		t.Fatal("report: Fresh = false для первой жалобы")
	}
	// данные для уведомления в канал модерации собраны из сообщения и автора
	if rep.Text != "гадость" || rep.AuthorTgID != author.TgID || rep.AuthorName != "Author" {
		t.Fatalf("report: %+v", rep)
	}

	// текст и автор скопированы — жалоба разбираема после удаления сообщения
	var gotText, gotReason string
	var gotAuthor int64
	if err := s.db.QueryRow(
		`SELECT message_text, author_tg_id, reason FROM reports WHERE message_id = ?`, msgID).
		Scan(&gotText, &gotAuthor, &gotReason); err != nil {
		t.Fatalf("select report: %v", err)
	}
	if gotText != "гадость" || gotAuthor != author.TgID || gotReason != "abuse" {
		t.Fatalf("report: text=%q author=%d reason=%q", gotText, gotAuthor, gotReason)
	}

	// повторная жалоба того же пользователя — успех, но без дубля; Fresh=false,
	// чтобы не постить в канал модерации второй раз
	if again, err := s.ReportMessage(msgID, reporter.TgID, "spam"); err != nil || again == nil {
		t.Fatalf("report again: rep=%v err=%v", again, err)
	} else if again.Fresh {
		t.Fatal("report again: Fresh = true для повторной жалобы")
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM reports WHERE message_id = ?`, msgID).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("report дублируется: %d записей", count)
	}

	// жалоба на несуществующее сообщение — nil, но не ошибка
	if missing, err := s.ReportMessage(9999, reporter.TgID, "spam"); err != nil || missing != nil {
		t.Fatalf("report missing: rep=%v err=%v", missing, err)
	}

	// жалоба переживает удаление самого сообщения (TTL-уборка)
	if _, err := s.DeleteMessagesOlderThan(0); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM reports`).Scan(&count); err != nil {
		t.Fatalf("count after cleanup: %v", err)
	}
	if count != 1 {
		t.Fatalf("жалоба пропала после удаления сообщения: %d", count)
	}
}

func TestStorePushTargets(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	// автор (1), сосед по каналу с двумя устройствами (2), человек из другого
	// района (3) — пуш должен уйти только устройствам второго
	for _, u := range []User{{TgID: 1, FullName: "Author"}, {TgID: 2, FullName: "Neighbour"}, {TgID: 3, FullName: "Far"}} {
		if err := s.CreateUser(u); err != nil {
			t.Fatalf("create %d: %v", u.TgID, err)
		}
	}
	if err := s.SetUserChannels(1, []string{"RU", "RU-MOW", "relation/1"}); err != nil {
		t.Fatalf("channels 1: %v", err)
	}
	if err := s.SetUserChannels(2, []string{"RU", "RU-MOW", "relation/1"}); err != nil {
		t.Fatalf("channels 2: %v", err)
	}
	if err := s.SetUserChannels(3, []string{"RU", "relation/999"}); err != nil {
		t.Fatalf("channels 3: %v", err)
	}
	for _, d := range []struct {
		tg  int64
		tok string
	}{
		{1, "dev-author"}, {2, "dev-n1"}, {2, "dev-n2"}, {3, "dev-far"},
	} {
		if err := s.SaveDeviceToken(d.tg, d.tok, "android"); err != nil {
			t.Fatalf("token %s: %v", d.tok, err)
		}
	}

	// автор исключён — это и есть суть адресной модели (в топик так было нельзя)
	got, err := s.PushTargets("relation/1", 1)
	if err != nil {
		t.Fatalf("targets: %v", err)
	}
	want := map[string]bool{"dev-n1": true, "dev-n2": true}
	if len(got) != len(want) {
		t.Fatalf("targets: %v, want %v", got, want)
	}
	for _, tok := range got {
		if !want[tok] {
			t.Fatalf("лишний получатель %q (автор или чужой район): %v", tok, got)
		}
	}

	// переезд: набор каналов заменяется целиком, из старого района пуши больше
	// не приходят
	if err := s.SetUserChannels(2, []string{"RU", "relation/777"}); err != nil {
		t.Fatalf("move: %v", err)
	}
	if got, err := s.PushTargets("relation/1", 1); err != nil || len(got) != 0 {
		t.Fatalf("после переезда targets=%v err=%v, want пусто", got, err)
	}

	// токен переезжает между аккаунтами (переустановка/смена аккаунта), не дублируясь
	if err := s.SaveDeviceToken(3, "dev-n1", "ios"); err != nil {
		t.Fatalf("re-bind: %v", err)
	}
	got, err = s.PushTargets("relation/999", 1)
	if err != nil {
		t.Fatalf("targets after re-bind: %v", err)
	}
	if len(got) != 2 { // dev-far + переехавший dev-n1
		t.Fatalf("targets after re-bind: %v, want 2", got)
	}

	// мёртвые токены убираются (FCM ответил UNREGISTERED)
	if err := s.DeleteDeviceTokens([]string{"dev-far", "dev-n1"}); err != nil {
		t.Fatalf("delete stale: %v", err)
	}
	if got, err := s.PushTargets("relation/999", 1); err != nil || len(got) != 0 {
		t.Fatalf("после уборки targets=%v err=%v, want пусто", got, err)
	}

	// удаление аккаунта уносит его токены и каналы (ON DELETE CASCADE)
	if err := s.SaveDeviceToken(2, "dev-n3", "android"); err != nil {
		t.Fatalf("token: %v", err)
	}
	if err := s.DeleteUser(2); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM device_tokens WHERE tg_id = 2`).Scan(&n); err != nil {
		t.Fatalf("count tokens: %v", err)
	}
	if n != 0 {
		t.Fatalf("после удаления аккаунта осталось %d токенов", n)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM user_channels WHERE tg_id = 2`).Scan(&n); err != nil {
		t.Fatalf("count channels: %v", err)
	}
	if n != 0 {
		t.Fatalf("после удаления аккаунта осталось %d каналов", n)
	}
}
