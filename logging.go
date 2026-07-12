package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// consoleHandler — компактный человекочитаемый формат логов:
//
//	LEVEL [component] message  key=value ...
//
// Без времени: под systemd метку времени ставит сам journald, дублировать её в
// строке незачем (при локальном запуске время видно по промпту/`ts`).
//
// [component] определяется автоматически по имени файла места вызова (весь
// сервер — один пакет main, поэтому имя пакета бесполезно, а файлы разбиты по
// подсистемам: client.go → [client], rest.go → [rest]). Стандартный
// slog.TextHandler так не умеет: он всегда печатает `level=`/`msg=` через
// key=value.
type consoleHandler struct {
	mu    *sync.Mutex
	w     io.Writer
	level slog.Level
	attrs []slog.Attr
}

func newConsoleHandler(w io.Writer, level slog.Level) *consoleHandler {
	return &consoleHandler{mu: &sync.Mutex{}, w: w, level: level}
}

func (h *consoleHandler) Enabled(_ context.Context, l slog.Level) bool {
	return l >= h.level
}

func (h *consoleHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	nh := *h
	nh.attrs = append(append([]slog.Attr{}, h.attrs...), attrs...)
	return &nh
}

// Группы не используем — возвращаем себя без изменений.
func (h *consoleHandler) WithGroup(string) slog.Handler { return h }

func (h *consoleHandler) Handle(_ context.Context, r slog.Record) error {
	var pairs []string
	appendAttr := func(a slog.Attr) {
		v := a.Value.String()
		if v == "" || strings.ContainsAny(v, " \t\"") {
			v = fmt.Sprintf("%q", v)
		}
		pairs = append(pairs, a.Key+"="+v)
	}
	for _, a := range h.attrs {
		appendAttr(a)
	}
	r.Attrs(func(a slog.Attr) bool { appendAttr(a); return true })

	// component — имя файла места вызова (slog кладёт PC вызывающего в
	// Record.PC): client.go → "client", rest.go → "rest".
	var component string
	if r.PC != 0 {
		fs := runtime.CallersFrames([]uintptr{r.PC})
		if f, _ := fs.Next(); f.File != "" {
			component = strings.TrimSuffix(filepath.Base(f.File), ".go")
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%-5s ", r.Level.String()) // INFO / WARN / ERROR, ширина под выравнивание
	if component != "" {
		fmt.Fprintf(&b, "[%s] ", component)
	}
	b.WriteString(r.Message)
	for _, p := range pairs {
		b.WriteByte(' ')
		b.WriteString(p)
	}
	b.WriteByte('\n')

	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.w, b.String())
	return err
}
