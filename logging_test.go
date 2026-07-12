package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestConsoleHandlerFormat(t *testing.T) {
	var buf bytes.Buffer
	l := slog.New(newConsoleHandler(&buf, slog.LevelInfo))

	// component автоматически = имя файла вызова (здесь logging_test)
	l.Info("listening", "addr", ":8080")
	l.Warn("bad", "err", "bad handshake") // значение с пробелом → в кавычки

	got := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	want := []string{
		"INFO  [logging_test] listening addr=:8080",
		`WARN  [logging_test] bad err="bad handshake"`,
	}
	if len(got) != len(want) {
		t.Fatalf("строк: got %d, want %d\n%s", len(got), len(want), buf.String())
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("строка %d:\n got %q\nwant %q", i, got[i], want[i])
		}
	}
}

func TestConsoleHandlerLevelFilter(t *testing.T) {
	var buf bytes.Buffer
	l := slog.New(newConsoleHandler(&buf, slog.LevelInfo))
	l.Debug("noisy") // ниже Info — не должно попасть в вывод
	if buf.Len() != 0 {
		t.Fatalf("Debug просочился при уровне Info: %q", buf.String())
	}
}
