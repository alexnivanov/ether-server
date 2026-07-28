package main

import (
	"context"
	"log/slog"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/getsentry/sentry-go"
)

// Ошибки и паники — в Sentry. Почему он, а не самодельная отправка ERROR в
// служебный Telegram-канал: Sentry группирует однотипные ошибки (одна проблема =
// одна запись, а не сотня сообщений), показывает стектрейс и ловит паники,
// которых мы иначе вообще не увидим — net/http гасит их внутри соединения.
// Telegram-канал остаётся для продуктовых событий (жалобы, регистрации).
//
// Опционален, как FCM и уведомления: без `sentry_dsn` в конфиге ничего не
// инициализируется и сервер работает как раньше.
//
// ПРИВАТНОСТЬ: в сообщениях ошибок есть `tg_id` — идентификатор пользователя.
// Оставляем сознательно (без него разбор невозможен), поэтому Sentry указан в
// privacy policy как сторонний сервис. Тела запросов, заголовки и IP не
// отправляем: SendDefaultPII выключен.

// initSentry настраивает клиент. Возвращает функцию сброса буфера — вызвать
// перед выходом, иначе последние события не успеют уйти. Пустой DSN →
// (no-op, nil): Sentry выключен.
func initSentry(dsn, env, release string) (flush func(), err error) {
	if dsn == "" {
		return func() {}, nil
	}
	if err := sentry.Init(sentry.ClientOptions{
		Dsn:         dsn,
		Environment: env,     // dev | prod — чтобы локальные прогоны не мешались с продом
		Release:     release, // версия из -ldflags: видно, в какой сборке появилась ошибка
		// Трейсинг не включаем: нужны ошибки и паники, а не профиль
		// производительности — иначе бесплатная квота уйдёт на трейсы.
		EnableTracing: false,
		// Не отправлять IP, заголовки и тела запросов. Идентификаторы
		// пользователей всё равно попадут через атрибуты логов (см. выше).
		SendDefaultPII: false,
	}); err != nil {
		return func() {}, err
	}
	return func() { sentry.Flush(2 * time.Second) }, nil
}

// sentryHandler — декоратор над обычным slog-хендлером: пишет как раньше в
// консоль/journald и дополнительно отправляет записи уровня ERROR в Sentry.
//
// Декоратор, а не отдельные вызовы sentry.CaptureException по коду: места ошибок
// уже логируются через slog, дублировать их вызовами SDK — значит однажды
// забыть один из двух.
type sentryHandler struct {
	inner   slog.Handler
	attrs   []slog.Attr
	capture func(r slog.Record, attrs []slog.Attr) // подменяется в тестах
}

func newSentryHandler(inner slog.Handler) *sentryHandler {
	return &sentryHandler{inner: inner, capture: captureToSentry}
}

func (h *sentryHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h *sentryHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	nh := *h
	nh.inner = h.inner.WithAttrs(attrs)
	nh.attrs = append(append([]slog.Attr{}, h.attrs...), attrs...)
	return &nh
}

func (h *sentryHandler) WithGroup(name string) slog.Handler {
	nh := *h
	nh.inner = h.inner.WithGroup(name)
	return &nh
}

func (h *sentryHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level >= slog.LevelError {
		h.capture(r, h.attrs)
	}
	return h.inner.Handle(ctx, r)
}

// captureToSentry превращает запись лога в событие Sentry.
func captureToSentry(r slog.Record, base []slog.Attr) {
	ev := sentry.NewEvent()
	ev.Level = sentry.LevelError
	ev.Message = r.Message
	// Фингерпринт по сообщению: у нас сообщения — короткие стабильные строки
	// («fcm send», «save message»), а переменная часть живёт в атрибутах. Так
	// одна проблема остаётся одной записью, сколько бы разных tg_id в неё ни
	// попало.
	ev.Fingerprint = []string{r.Message}
	// Атрибуты лога — в Contexts (поле Extra в sentry-go 0.48 убрали).
	fields := make(sentry.Context, 8)

	add := func(a slog.Attr) {
		// err выносим в само сообщение: по нему сразу видно текст сбоя
		if a.Key == "err" {
			ev.Message = r.Message + ": " + a.Value.String()
			return
		}
		fields[a.Key] = a.Value.Any()
	}
	for _, a := range base {
		add(a)
	}
	r.Attrs(func(a slog.Attr) bool { add(a); return true })

	if len(fields) > 0 {
		ev.Contexts = map[string]sentry.Context{"log": fields}
	}

	// component (имя файла) — тегом: в Sentry по нему можно фильтровать
	if r.PC != 0 {
		fs := runtime.CallersFrames([]uintptr{r.PC})
		if f, _ := fs.Next(); f.File != "" {
			ev.Tags = map[string]string{
				"component": strings.TrimSuffix(filepath.Base(f.File), ".go"),
			}
		}
	}
	sentry.CaptureEvent(ev)
}
