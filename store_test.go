package main

import (
	"path/filepath"
	"testing"
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
