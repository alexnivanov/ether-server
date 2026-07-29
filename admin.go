package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Команды модерации в служебном Telegram-канале. Жалобы туда уже приходят
// (notify.go), поэтому и реакция должна быть там же: отвечаешь с телефона, id
// автора и сообщения в посте жалобы копируются тапом.
//
// Авторизация бесплатная и надёжная: принимаем команды ТОЛЬКО из настроенного
// чата, а писать в канал могут лишь его администраторы. Никаких своих токенов и
// паролей не вводим.
//
// Команды (причина необязательна, её увидит пользователь):
//
//	/ban <id> [причина]    закрыть отправку на сутки; читать и заходить можно
//	/block <id> [причина]  заблокировать навсегда + удалить аккаунт
//	/unban <id>            снять наказание
//	/del <msg_id>          удалить одно сообщение
//	/purge <id>            удалить все сообщения пользователя
//	/help                     список команд
//
// Автоматической эскалации нет: в посте жалобы и в ответе на команду видно, сколько
// раз человек уже попадался, а решение — мьют или блокировка — принимает модератор.
type AdminBot struct {
	notify *Notifier // токен, chat_id и HTTP-клиент; сюда же отвечаем
	store  *Store
	offset int64 // update_id + 1: подтверждает обработанные обновления
}

// NewAdminBot возвращает nil, если уведомления выключены: без токена и канала
// команды принимать неоткуда.
func NewAdminBot(notify *Notifier, store *Store) *AdminBot {
	if notify == nil {
		return nil
	}
	return &AdminBot{notify: notify, store: store}
}

// Run в бесконечном цикле забирает обновления бота (long-poll) и исполняет
// команды. Запускать в отдельной горутине.
//
// ВАЖНО: getUpdates у бота один на всех — если когда-нибудь поставим вебхук,
// опрос начнёт возвращать ошибку (это будет видно в логе).
func (a *AdminBot) Run() {
	a.skipBacklog()
	for {
		updates, err := a.getUpdates(25 * time.Second)
		if err != nil {
			slog.Warn("admin getUpdates", "err", err)
			time.Sleep(5 * time.Second) // не долбить Telegram при сбое
			continue
		}
		for _, u := range updates {
			a.offset = u.UpdateID + 1
			msg := u.ChannelPost
			if msg == nil {
				msg = u.Message
			}
			if msg == nil || msg.Text == "" {
				continue
			}
			// чужие чаты (в т.ч. личка бота) игнорируем: команды только из
			// служебного канала
			if strconv.FormatInt(msg.Chat.ID, 10) != a.notify.chatID {
				continue
			}
			if reply := a.execute(msg.Text); reply != "" {
				a.notify.send("admin", reply, "cmd", firstWord(msg.Text))
			}
		}
	}
}

// skipBacklog подтверждает всё, что накопилось до старта, НЕ исполняя. Иначе
// рестарт сервера повторно выполнил бы старые команды: `/block` удалил бы уже
// восстановленный аккаунт, а каждый повтор `/ban` накручивал бы счётчик
// нарушений и продлевал мьют.
func (a *AdminBot) skipBacklog() {
	updates, err := a.getUpdates(0)
	if err != nil {
		slog.Warn("admin skip backlog", "err", err)
		return
	}
	for _, u := range updates {
		if u.UpdateID >= a.offset {
			a.offset = u.UpdateID + 1
		}
	}
	if len(updates) > 0 {
		slog.Info("admin backlog skipped", "count", len(updates), "offset", a.offset)
	}
}

type tgUpdate struct {
	UpdateID    int64      `json:"update_id"`
	Message     *tgMessage `json:"message"`
	ChannelPost *tgMessage `json:"channel_post"`
}

type tgMessage struct {
	Text string `json:"text"`
	Chat struct {
		ID int64 `json:"id"`
	} `json:"chat"`
}

func (a *AdminBot) getUpdates(timeout time.Duration) ([]tgUpdate, error) {
	q := url.Values{}
	q.Set("offset", strconv.FormatInt(a.offset, 10))
	q.Set("timeout", strconv.Itoa(int(timeout.Seconds())))
	q.Set("allowed_updates", `["message","channel_post"]`)
	endpoint := fmt.Sprintf("https://api.telegram.org/bot%s/getUpdates?%s",
		a.notify.token, q.Encode())

	// клиент Notifier'а с таймаутом 10с не годится для long-poll — свой запрос
	client := &http.Client{Timeout: timeout + 15*time.Second}
	resp, err := client.Get(endpoint)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		OK          bool       `json:"ok"`
		Description string     `json:"description"`
		Result      []tgUpdate `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if !out.OK {
		return nil, fmt.Errorf("telegram: %s", out.Description)
	}
	return out.Result, nil
}

// execute исполняет одну команду и возвращает ответ для канала (пустая строка —
// это не команда, промолчать). Вынесено из цикла опроса: HTTP тут не участвует,
// поэтому логика тестируется напрямую.
func (a *AdminBot) execute(text string) string {
	fields := strings.Fields(strings.TrimSpace(text))
	if len(fields) == 0 || !strings.HasPrefix(fields[0], "/") {
		return "" // обычное сообщение в канале — не наше дело
	}
	// в группах Telegram дописывает к команде имя бота: /ban@ether_app_bot
	cmd := strings.ToLower(strings.SplitN(fields[0], "@", 2)[0])
	args := fields[1:]

	// Сначала убеждаемся, что команда наша: в канале могут быть и чужие слэш-
	// команды, и на них нельзя отвечать «нужен аргумент» — иначе бот огрызается
	// на всё подряд. Только потом разбираем id.
	needsID := map[string]bool{"/ban": true, "/block": true, "/unban": true, "/del": true, "/purge": true}
	if !needsID[cmd] && cmd != "/help" {
		return ""
	}
	var id int64
	var reason string
	if needsID[cmd] {
		if len(args) == 0 {
			return fmt.Sprintf("Нужен id: <code>%s &lt;id&gt;</code>", cmd)
		}
		n, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil || n <= 0 {
			return fmt.Sprintf("Не похоже на id: <code>%s</code>", html.EscapeString(args[0]))
		}
		id = n
		// остаток строки — причина (необязательная): её увидит пользователь
		reason = strings.Join(args[1:], " ")
	}

	switch cmd {
	case "/ban":
		until, count, err := a.store.BanTemporary(id, reason)
		if errors.Is(err, ErrNoIdentities) {
			return fmt.Sprintf("Пользователя <code>%d</code> нет — аккаунт уже удалён", id)
		}
		if err != nil {
			slog.Error("admin ban", "err", err, "user_id", id)
			return "Не удалось закрыть отправку — смотри логи сервера"
		}
		slog.Info("moderation ban", "user_id", id, "until", until, "count", count, "reason", reason)
		return fmt.Sprintf(
			"⏳ <code>%d</code> — отправка закрыта на сутки, до %s%s\nНарушений всего: <b>%d</b>. Если мало — <code>/block %d</code>",
			id, until.Format("02.01 15:04"), reasonSuffix(reason), count, id)

	case "/block":
		count, err := a.store.BanPermanent(id, reason)
		if errors.Is(err, ErrNoIdentities) {
			return fmt.Sprintf("Пользователя <code>%d</code> нет — аккаунт уже удалён", id)
		}
		if err != nil {
			slog.Error("admin block", "err", err, "user_id", id)
			return "Не удалось заблокировать — смотри логи сервера"
		}
		slog.Info("moderation block", "user_id", id, "count", count, "reason", reason)
		return fmt.Sprintf(
			"⛔️ <code>%d</code> — заблокирован навсегда, аккаунт и его сообщения удалены%s\nНарушений всего: <b>%d</b>. Вход закрыт даже после переустановки",
			id, reasonSuffix(reason), count)

	case "/unban":
		ok, err := a.store.Unban(id)
		if err != nil {
			slog.Error("admin unban", "err", err, "user_id", id)
			return "Не удалось снять бан — смотри логи сервера"
		}
		if !ok {
			return fmt.Sprintf("У <code>%d</code> нет активного наказания", id)
		}
		slog.Info("moderation unban", "user_id", id)
		return fmt.Sprintf(
			"✅ <code>%d</code> — наказание снято. Счётчик нарушений сохранён: ты будешь видеть его при следующей жалобе",
			id)

	case "/del":
		ok, err := a.store.DeleteMessage(id)
		if err != nil {
			slog.Error("admin del", "err", err, "message_id", id)
			return "Не удалось удалить — смотри логи сервера"
		}
		if !ok {
			return fmt.Sprintf("Сообщения <code>%d</code> нет — уже удалено или истёк срок хранения", id)
		}
		slog.Info("moderation delete message", "message_id", id)
		return fmt.Sprintf("🗑 Сообщение <code>%d</code> удалено", id)

	case "/purge":
		n, err := a.store.DeleteUserMessages(id)
		if err != nil {
			slog.Error("admin purge", "err", err, "user_id", id)
			return "Не удалось очистить — смотри логи сервера"
		}
		slog.Info("moderation purge", "user_id", id, "deleted", n)
		return fmt.Sprintf("🗑 Удалено сообщений пользователя <code>%d</code>: %d", id, n)

	case "/help":
		return "<b>Команды модерации</b>\n" +
			"<code>/ban &lt;id&gt; [причина]</code> — закрыть отправку на сутки (читать можно)\n" +
			"<code>/block &lt;id&gt; [причина]</code> — заблокировать навсегда + удалить аккаунт\n" +
			"<code>/unban &lt;id&gt;</code> — снять наказание\n" +
			"<code>/del &lt;msg_id&gt;</code> — удалить сообщение\n" +
			"<code>/purge &lt;id&gt;</code> — удалить все сообщения пользователя\n\n" +
			"Причина необязательна, но её увидит пользователь. Эскалации нет: " +
			"счётчик нарушений показывается, решение за тобой."

	default:
		return "" // незнакомые команды молча пропускаем: канал не только для нас
	}
}

// reasonSuffix добавляет причину к ответу, если она указана.
func reasonSuffix(reason string) string {
	if reason == "" {
		return ""
	}
	return ". Причина: " + html.EscapeString(reason)
}

func firstWord(s string) string {
	if f := strings.Fields(s); len(f) > 0 {
		return f[0]
	}
	return ""
}
