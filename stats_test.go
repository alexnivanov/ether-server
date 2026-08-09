package main

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// failingTransport — Telegram недоступен: send обязан вернуть ошибку, а
// вызывающий — не поставить отметку об отправке.
type failingTransport struct{ calls int }

func (f *failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	f.calls++
	return nil, errors.New("сеть недоступна")
}

// TestLastScheduled — какой момент расписания считается «ближайшим прошедшим».
// Ошибка здесь означает либо сводку не за ту неделю, либо две сводки подряд.
func TestLastScheduled(t *testing.T) {
	// 8 августа 2026 — суббота
	sat := time.Date(2026, 8, 8, 10, 0, 0, 0, time.Local)

	cases := []struct {
		name string
		now  time.Time
		want time.Time
	}{
		{"ровно в момент расписания", sat, sat},
		{"суббота, часом позже", sat.Add(time.Hour), sat},
		{"суббота, но 10:00 ещё не наступило", sat.Add(-2 * time.Hour), sat.AddDate(0, 0, -7)},
		{"воскресенье", sat.AddDate(0, 0, 1), sat},
		{"пятница — отчитываемся за прошлую субботу", sat.AddDate(0, 0, 6), sat},
		{"ровно через неделю", sat.AddDate(0, 0, 7), sat.AddDate(0, 0, 7)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := lastScheduled(c.now); !got.Equal(c.want) {
				t.Errorf("lastScheduled(%s) = %s, want %s", c.now, got, c.want)
			}
		})
	}
}

// TestWeeklyStatsSentOnce — сервер перезапускается часто, а проверка идёт раз в
// час: за один момент расписания сводка обязана уйти ровно один раз.
func TestWeeklyStatsSentOnce(t *testing.T) {
	store := openTestStore(t)
	notify, sent := newFakeNotifier()
	now := time.Now()

	for i := 0; i < 3; i++ {
		if err := sendWeeklyStatsIfDue(store, notify, now); err != nil {
			t.Fatalf("проход %d: %v", i, err)
		}
	}

	if len(sent) != 1 {
		t.Fatalf("отправлено %d сводок, want 1", len(sent))
	}
	body, _ := (<-sent)["text"].(string)
	if !strings.Contains(body, "Эфир за неделю") {
		t.Errorf("текст сводки неожиданный: %q", body)
	}
}

// TestWeeklyStatsRetriesAfterFailure — Telegram недоступен. Отметка не должна
// встать: иначе одна сетевая ошибка навсегда съедает недельный отчёт, и узнать
// об этом будет неоткуда.
func TestWeeklyStatsRetriesAfterFailure(t *testing.T) {
	store := openTestStore(t)
	tr := &failingTransport{}
	broken := &Notifier{token: "t", chatID: "-1", http: &http.Client{Transport: tr}}
	now := time.Now()

	if err := sendWeeklyStatsIfDue(store, broken, now); err == nil {
		t.Fatal("ожидалась ошибка отправки")
	}
	if sent, err := store.WeeklyStatsSent(lastScheduled(now).UnixMilli()); err != nil {
		t.Fatalf("проверка отметки: %v", err)
	} else if sent {
		t.Fatal("отметка встала при неудачной отправке — сводка за неделю потеряна")
	}

	// следующая попытка (через час) должна отправить, и вот теперь — отметить
	notify, sentCh := newFakeNotifier()
	if err := sendWeeklyStatsIfDue(store, notify, now); err != nil {
		t.Fatalf("повторная отправка: %v", err)
	}
	if len(sentCh) != 1 {
		t.Fatalf("отправлено %d сводок, want 1", len(sentCh))
	}
	if sent, _ := store.WeeklyStatsSent(lastScheduled(now).UnixMilli()); !sent {
		t.Error("после успешной отправки отметки нет")
	}
}

// TestWeeklyStatsQuery — сами цифры: окно полуоткрытое, группировки считают то,
// что нужно, и заход без приглашающего не пропадает из разбивки по источникам.
func TestWeeklyStatsQuery(t *testing.T) {
	store := openTestStore(t)
	alex := mkTgUser(t, store, "1", "alex", "Алексей")

	now := time.Now().UnixMilli()
	from, to := now-1000, now+1000

	must := func(a AppAccess) {
		t.Helper()
		if err := store.SaveAppAccess(a); err != nil {
			t.Fatalf("save app access: %v", err)
		}
	}
	must(AppAccess{UID: alex, Src: "apli", Platform: platformIOS, Outcome: outcomeAppStore})
	must(AppAccess{UID: alex, Src: "apqr", Platform: platformIOS, Outcome: outcomeAppStore})
	must(AppAccess{Src: "", Platform: platformDesktop, Outcome: outcomeLanding})

	st, err := store.WeeklyStats(from, to)
	if err != nil {
		t.Fatalf("weekly stats: %v", err)
	}

	if st.Accesses != 3 {
		t.Errorf("Accesses = %d, want 3", st.Accesses)
	}
	if st.NewUsers != 1 || st.TotalUsers != 1 {
		t.Errorf("NewUsers/TotalUsers = %d/%d, want 1/1", st.NewUsers, st.TotalUsers)
	}
	// заход без src обязан остаться видимой строкой, а не выпасть из группировки
	if got := groupCount(st.BySrc, "без источника"); got != 1 {
		t.Errorf("BySrc[без источника] = %d, want 1", got)
	}
	if got := groupCount(st.BySrc, "apli"); got != 1 {
		t.Errorf("BySrc[apli] = %d, want 1", got)
	}
	if got := groupCount(st.ByPlatform, platformIOS); got != 2 {
		t.Errorf("ByPlatform[ios] = %d, want 2", got)
	}
	// в «кто позвал» попадают только заходы с uid, и имя берётся из users
	if len(st.ByInviter) != 1 || st.ByInviter[0].Key != "Алексей" || st.ByInviter[0].Count != 2 {
		t.Errorf("ByInviter = %+v, want [{Алексей 2}]", st.ByInviter)
	}
	if len(st.AccessRows) != 3 {
		t.Errorf("AccessRows = %d, want 3", len(st.AccessRows))
	}

	// окно полуоткрытое: строка ровно на верхней границе относится к следующей
	// неделе, иначе один и тот же заход попал бы в две сводки
	st, err = store.WeeklyStats(from, st.AccessRows[0].TS)
	if err != nil {
		t.Fatalf("weekly stats: %v", err)
	}
	if st.Accesses != 0 {
		t.Errorf("Accesses = %d на границе окна, want 0", st.Accesses)
	}
}

func groupCount(rows []CountRow, key string) int {
	for _, r := range rows {
		if r.Key == key {
			return r.Count
		}
	}
	return 0
}

// TestFormatWeeklyStatsTruncates — упереться в лимит Telegram нельзя: сообщение
// отвергается, отметка не встаёт, а на следующей неделе строк становится ещё
// больше — сводка перестала бы выходить навсегда.
func TestFormatWeeklyStatsTruncates(t *testing.T) {
	st := &WeeklyStats{Accesses: 500}
	for i := 0; i < 500; i++ {
		st.AccessRows = append(st.AccessRows, AccessRow{
			TS: time.Now().UnixMilli(), Src: "apli", Platform: platformIOS,
			Outcome: outcomeAppStore, InviterName: "Алексей",
			UA: "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) Safari/604.1",
		})
	}

	text := formatWeeklyStats(st, time.Now().AddDate(0, 0, -7), time.Now())

	if len([]rune(text)) >= 4096 {
		t.Errorf("длина сводки %d рун — лимит Telegram 4096", len([]rune(text)))
	}
	if !strings.Contains(text, "Показаны первые") {
		t.Error("список обрезан молча — в сводке нет отметки о срезке")
	}
}

// TestFormatWeeklyStatsQuietWeek — за тихую неделю сводка всё равно приходит:
// её отсутствие неотличимо от сломавшегося отчёта.
func TestFormatWeeklyStatsQuietWeek(t *testing.T) {
	text := formatWeeklyStats(&WeeklyStats{TotalUsers: 7},
		time.Now().AddDate(0, 0, -7), time.Now())

	if !strings.Contains(text, "Аккаунты: +0 (всего 7)") {
		t.Errorf("нет строки про аккаунты: %q", text)
	}
	// пустых заголовков групп быть не должно
	if strings.Contains(text, "Источник:") || strings.Contains(text, "Переходы</b>") {
		t.Errorf("пустые группы попали в сводку: %q", text)
	}
}

// TestShortUA — сырой UA в списке из сорока строк нечитаем, а превью ссылок в
// мессенджерах надо отличать от людей с одного взгляда.
func TestShortUA(t *testing.T) {
	cases := map[string]string{
		uaIPhone:                        "iPhone",
		uaAndroid:                       "Android",
		uaDesktop:                       "Mac",
		"TelegramBot (like TwitterBot)": "превью TG",
		"WhatsApp/2.23":                 "превью WA",
		"facebookexternalhit/1.1":       "превью FB",
		"Mozilla/5.0 (Windows NT 10.0; Win64) Chrome/126.0": "Windows",
		"":           "-",
		"curl/8.4.0": "прочее",
	}
	for ua, want := range cases {
		if got := shortUA(ua); got != want {
			t.Errorf("shortUA(%q) = %q, want %q", ua, got, want)
		}
	}
}

// TestSendWeeklyStatsIfDueWithoutNotifier — уведомления не настроены (notify ==
// nil): сводку отправлять некуда, но и падать не должно.
func TestSendWeeklyStatsIfDueWithoutNotifier(t *testing.T) {
	store := openTestStore(t)
	if err := sendWeeklyStatsIfDue(store, nil, time.Now()); err != nil {
		t.Fatalf("без notifier: %v", err)
	}
}
