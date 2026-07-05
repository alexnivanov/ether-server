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

	u := User{TgID: 42, Username: "alex", FirstName: "Alex", Nick: "alex"}
	if err := s.SaveUser(u); err != nil {
		t.Fatalf("save: %v", err)
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
	u.Nick = "alex_new"
	if err := s.SaveUser(u); err != nil {
		t.Fatalf("re-save: %v", err)
	}
	got, err = s.UserBySession(token)
	if err != nil {
		t.Fatalf("resume after re-save: %v", err)
	}
	if got == nil || got.Nick != "alex_new" {
		t.Fatalf("resume after re-save: got %+v, want nick alex_new", got)
	}
}
