package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Notifier отправляет служебные уведомления в Telegram-канал: жалобы на
// сообщения и регистрации новых пользователей приходят туда сразу, а не ждут,
// пока кто-то заглянет в journalctl. Отдельный бот-токен и chat_id канала — в
// конфиге; оба пусты → уведомления выключены (событие всё равно в БД и в логе).
//
// Это ЕДИНСТВЕННОЕ место, где серверу нужен секретный токен бота: вход
// пользователей проверяется по публичным ключам Telegram (JWKS, см.
// telegram.go). Требует исходящего доступа к api.telegram.org — на хостах, где
// Telegram заблокирован, уведомления просто не уйдут (ошибка в лог, жалоба
// сохранится).
type Notifier struct {
	token  string
	chatID string
	http   *http.Client
}

// NewNotifier возвращает nil, если токен или chat_id не заданы — вызывающий
// трактует nil как «уведомления выключены» (как Pusher в push.go).
func NewNotifier(token, chatID string) *Notifier {
	if token == "" || chatID == "" {
		return nil
	}
	return &Notifier{
		token:  token,
		chatID: chatID,
		http:   &http.Client{Timeout: 10 * time.Second},
	}
}

// maxNotifyText — сколько символов текста сообщения кладём в уведомление.
// Лимит Telegram на сообщение — 4096, до него далеко, но простыню в канал
// модерации лить незачем: для решения хватает начала.
const maxNotifyText = 500

// ReportToChannel шлёт жалобу в служебный канал. Задумана для вызова в горутине:
// блокируется на HTTP к Telegram, а ответ клиенту от уведомления не зависит —
// жалоба уже записана в БД, поэтому ошибки только логируем.
func (n *Notifier) ReportToChannel(rep *ReportedMessage, reporter *User) {
	author := rep.AuthorName
	if rep.AuthorUsername != "" {
		author += " @" + rep.AuthorUsername
	}
	if strings.TrimSpace(author) == "" {
		author = "без имени"
	}
	from := reporter.FullName
	if reporter.TgUsername != "" {
		from += " @" + reporter.TgUsername
	}

	text := rep.Text
	if len(text) > maxNotifyText {
		// режем по рунам, чтобы не разорвать многобайтовый символ
		r := []rune(text)
		if len(r) > maxNotifyText {
			text = string(r[:maxNotifyText]) + "…"
		}
	}

	// HTML-разметка (parse_mode=HTML): экранируем всё пользовательское, иначе
	// "<" в сообщении сломает разбор и Telegram отклонит запрос
	body := fmt.Sprintf(
		"🚩 <b>Жалоба на сообщение</b>\n"+
			"Причина: <b>%s</b>\n"+
			"Автор: %s (<code>%d</code>)\n"+
			"Канал: <code>%s</code>\n"+
			"Сообщение #<code>%d</code>:\n<blockquote>%s</blockquote>\n"+
			"От: %s (<code>%d</code>)",
		html.EscapeString(rep.Reason),
		html.EscapeString(author), rep.AuthorTgID,
		html.EscapeString(rep.Channel),
		rep.MessageID, html.EscapeString(text),
		html.EscapeString(from), reporter.TgID,
	)

	n.send("report", body, "message_id", rep.MessageID)
}

// AccountCreated шлёт в служебный канал уведомление о регистрации нового
// пользователя (первый вход через Telegram, см. handleAuthTelegram). Как и
// ReportToChannel, задумана для вызова в горутине: ответ клиенту от неё не
// зависит, аккаунт уже создан.
func (n *Notifier) AccountCreated(u User) {
	who := u.FullName
	if u.TgUsername != "" {
		who += " @" + u.TgUsername
	}
	if strings.TrimSpace(who) == "" {
		who = "без имени"
	}
	body := fmt.Sprintf(
		"🎉 <b>Новый пользователь</b>\n%s (<code>%d</code>)",
		html.EscapeString(who), u.TgID,
	)
	n.send("account created", body, "tg_id", u.TgID)
}

// send отправляет готовый HTML-текст в служебный канал. kind и logAttrs — только
// для лога: ошибки здесь не возвращаются, потому что уведомление всегда
// вторично по отношению к самому событию (оно уже записано в БД).
func (n *Notifier) send(kind, body string, logAttrs ...any) {
	log := slog.With(logAttrs...)

	payload, _ := json.Marshal(map[string]any{
		"chat_id":                  n.chatID,
		"text":                     body,
		"parse_mode":               "HTML",
		"disable_web_page_preview": true,
	})
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", n.token)
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.http.Do(req)
	if err != nil {
		log.Error("notify "+kind, "err", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// тело Telegram объясняет причину (нет прав в канале, неверный chat_id)
		b, _ := io.ReadAll(resp.Body)
		log.Warn("notify "+kind+" rejected", "status", resp.Status, "body", string(b))
		return
	}
	log.Info("notify "+kind, "status", "ok")
}
