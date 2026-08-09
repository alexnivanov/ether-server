package main

import (
	"fmt"
	"html"
	"log/slog"
	"strings"
	"time"
)

// Еженедельная сводка в служебный Telegram-канал: сколько пришло людей, сколько
// написано сообщений и кто кого позвал по ссылке установки.
//
// Расписание — суббота, 10:00 по времени сервера. Проверка идёт РАЗ В ЧАС, а не
// сном до нужной минуты, и это принципиально: сервер перезапускается при каждом
// деплое, а таймер в памяти после рестарта начинает отсчёт заново, из-за чего
// сводка за неделю молча не выходит. Часовая проверка спрашивает у базы «за этот
// момент расписания уже отправляли?» (см. weekly_stats) и догоняет пропущенное —
// то же, что даёт systemd-таймеру Persistent=true, только без второго процесса.
const (
	statsWeekday  = time.Saturday
	statsHour     = 10
	statsInterval = time.Hour
)

// Границы построчного списка заходов. Лимит Telegram на сообщение — 4096
// символов, и упереться в него нельзя: sendMessage ответит ошибкой, отметка об
// отправке не встанет, а следующая неделя принесёт ещё больше строк — сводка
// перестала бы выходить навсегда. Поэтому список режется, а сколько строк
// осталось за кадром — пишется явно.
const (
	maxStatsRows = 40
	maxStatsLen  = 3500 // с запасом от 4096: заголовок и сводка уже посчитаны
)

// startWeeklyStats раз в statsInterval проверяет, не пора ли отправить сводку.
// Запускать в отдельной горутине (см. main). notify == nil — уведомления
// выключены, и запускать нечего.
func startWeeklyStats(store *Store, notify *Notifier) {
	for {
		if err := sendWeeklyStatsIfDue(store, notify, time.Now()); err != nil {
			slog.Error("weekly stats", "err", err)
		}
		time.Sleep(statsInterval)
	}
}

// sendWeeklyStatsIfDue отправляет сводку, если ближайший прошедший момент
// расписания ещё не отчитан. Вынесена из цикла ради тестов: время передаётся
// параметром, а не берётся из часов.
func sendWeeklyStatsIfDue(store *Store, notify *Notifier, now time.Time) error {
	due := lastScheduled(now)
	sent, err := store.WeeklyStatsSent(due.UnixMilli())
	if err != nil || sent {
		return err
	}
	from := due.AddDate(0, 0, -7)
	st, err := store.WeeklyStats(from.UnixMilli(), due.UnixMilli())
	if err != nil {
		return err
	}
	if err := notify.WeeklyStats(formatWeeklyStats(st, from, due)); err != nil {
		// Отметку не ставим: на следующем часу попробуем снова. Лучше сводка с
		// опозданием на час, чем её отсутствие без единого следа.
		return err
	}
	return store.MarkWeeklyStatsSent(due.UnixMilli())
}

// lastScheduled — ближайший НЕ БУДУЩИЙ момент расписания относительно now:
// последняя суббота 10:00 включительно. Время местное (у сервера — его
// собственная зона): сводка должна приходить в понятный час, а не в UTC.
func lastScheduled(now time.Time) time.Time {
	day := time.Date(now.Year(), now.Month(), now.Day(), statsHour, 0, 0, 0, now.Location())
	// сколько дней назад была нужная суббота; 0 — сегодня
	back := (int(day.Weekday()) - int(statsWeekday) + 7) % 7
	due := day.AddDate(0, 0, -back)
	if due.After(now) {
		// сегодня суббота, но 10:00 ещё не наступило — отчитываемся за прошлую
		due = due.AddDate(0, 0, -7)
	}
	return due
}

// formatWeeklyStats собирает текст сводки (HTML, как остальные уведомления).
func formatWeeklyStats(st *WeeklyStats, from, to time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "📊 <b>Эфир за неделю</b>\n%s — %s\n",
		from.Format("02.01"), to.Format("02.01"))

	fmt.Fprintf(&b, "\n👤 Аккаунты: +%d (всего %d)", st.NewUsers, st.TotalUsers)
	// «не меньше»: сообщения старше недели уже удалены уборщиком, и часть начала
	// периода в счёт не попадает (см. WeeklyStats)
	fmt.Fprintf(&b, "\n💬 Сообщений: не меньше %d в %d каналах", st.Messages, st.Channels)
	fmt.Fprintf(&b, "\n🔗 Переходов по ссылке: %d\n", st.Accesses)

	writeGroup(&b, "Источник", st.BySrc)
	writeGroup(&b, "Платформа", st.ByPlatform)
	writeGroup(&b, "Позвали", st.ByInviter)

	if len(st.AccessRows) == 0 {
		return b.String()
	}

	b.WriteString("\n<b>Переходы</b>\n<pre>")
	shown := 0
	for _, r := range st.AccessRows {
		if shown == maxStatsRows || b.Len() > maxStatsLen {
			break
		}
		fmt.Fprintf(&b, "%s | %s | %s | %s | %s\n",
			time.UnixMilli(r.TS).Format("02.01 15:04"),
			orDash(r.Src), inviter(r), r.Platform+"→"+r.Outcome, shortUA(r.UA))
		shown++
	}
	b.WriteString("</pre>")
	if shown < len(st.AccessRows) {
		fmt.Fprintf(&b, "\nПоказаны первые %d из %d", shown, len(st.AccessRows))
	}
	return b.String()
}

// writeGroup печатает группировку одной строкой: «Источник: apli 5, apqr 2».
// Пустая группа пропускается — в сводке за тихую неделю пустых заголовков быть
// не должно.
func writeGroup(b *strings.Builder, title string, rows []CountRow) {
	if len(rows) == 0 {
		return
	}
	parts := make([]string, 0, len(rows))
	for _, r := range rows {
		parts = append(parts, fmt.Sprintf("%s %d", html.EscapeString(r.Key), r.Count))
	}
	fmt.Fprintf(b, "\n%s: %s", title, strings.Join(parts, ", "))
}

// inviter — как показать позвавшего в строке списка: имя, если оно есть, иначе
// голый id (аккаунт мог быть удалён после перехода), иначе прочерк.
func inviter(r AccessRow) string {
	switch {
	case r.InviterName != "":
		return r.InviterName
	case r.UID > 0:
		return fmt.Sprintf("id %d", r.UID)
	default:
		return "-"
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// shortUA сжимает User-Agent до опознаваемого слова. Сырой UA — это строка на
// полтораста символов, и в списке из сорока строк он не читается вовсе.
//
// Отдельно названы сборщики превью и краулеры: они дёргают ссылку сами, попадают
// в app_access наравне с людьми, и при взгляде на список их надо отличать сразу.
func shortUA(ua string) string {
	switch {
	case ua == "":
		return "-"
	case strings.Contains(ua, "TelegramBot"):
		return "превью TG"
	case strings.Contains(ua, "WhatsApp"):
		return "превью WA"
	case strings.Contains(ua, "facebookexternalhit"):
		return "превью FB"
	case strings.Contains(ua, "http://www.google.com/bot.html"):
		return "краулер Google"
	case strings.Contains(ua, "bot") || strings.Contains(ua, "Bot"):
		return "бот"
	case strings.Contains(ua, "iPhone"):
		return "iPhone"
	case strings.Contains(ua, "iPad"):
		return "iPad"
	case strings.Contains(ua, "Android"):
		return "Android"
	case strings.Contains(ua, "Macintosh"):
		return "Mac"
	case strings.Contains(ua, "Windows NT"):
		return "Windows"
	default:
		return "прочее"
	}
}
