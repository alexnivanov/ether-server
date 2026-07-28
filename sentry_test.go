package main

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

// Хендлер-декоратор: в Sentry уходят только ERROR, всё остальное пишется как
// раньше. Проверяем без сети — capture подменяем.
func TestSentryHandlerCapturesOnlyErrors(t *testing.T) {
	var buf bytes.Buffer
	type captured struct {
		msg   string
		attrs map[string]string
	}
	var got []captured

	h := newSentryHandler(newConsoleHandler(&buf, slog.LevelInfo))
	h.capture = func(r slog.Record, base []slog.Attr) {
		c := captured{msg: r.Message, attrs: map[string]string{}}
		for _, a := range base {
			c.attrs[a.Key] = a.Value.String()
		}
		r.Attrs(func(a slog.Attr) bool {
			c.attrs[a.Key] = a.Value.String()
			return true
		})
		got = append(got, c)
	}
	log := slog.New(h)

	log.Info("обычное событие", "k", 1)
	log.Warn("предупреждение", "k", 2)
	if len(got) != 0 {
		t.Fatalf("info/warn ушли в Sentry: %+v", got)
	}

	log.Error("save message", "err", "disk full", "channel", "RU")
	if len(got) != 1 {
		t.Fatalf("ошибка не захвачена: %+v", got)
	}
	if got[0].msg != "save message" {
		t.Fatalf("сообщение = %q", got[0].msg)
	}
	if got[0].attrs["err"] != "disk full" || got[0].attrs["channel"] != "RU" {
		t.Fatalf("атрибуты = %+v", got[0].attrs)
	}

	// атрибуты из With() тоже должны попадать в событие — иначе теряется
	// контекст, добавленный на уровне подсистемы
	log.With("tg_id", int64(42)).Error("ban check", "err", "boom")
	if len(got) != 2 || got[1].attrs["tg_id"] != "42" {
		t.Fatalf("атрибуты из With() потеряны: %+v", got)
	}

	// в консоль при этом попало всё, включая info
	out := buf.String()
	for _, want := range []string{"обычное событие", "предупреждение", "save message", "ban check"} {
		if !strings.Contains(out, want) {
			t.Fatalf("в консоли нет %q:\n%s", want, out)
		}
	}
}

// Пустой DSN — Sentry выключен, flush не паникует.
func TestInitSentryDisabled(t *testing.T) {
	flush, err := initSentry("", "dev", "test")
	if err != nil {
		t.Fatalf("initSentry(\"\") = %v", err)
	}
	flush() // не должно ничего сделать и не должно упасть
}

// Мусорный DSN — ошибка возвращается, а не роняет старт сервера.
func TestInitSentryBadDSN(t *testing.T) {
	if _, err := initSentry("не-dsn", "dev", "test"); err == nil {
		t.Fatal("ожидали ошибку на мусорном DSN")
	}
	_ = context.Background()
}
