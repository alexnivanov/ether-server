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
	// 8 августа 2026 — суббота. Час берём из константы: расписание уже двигали
	// (10:00 → 7:00, чтобы сводка приходила в 10 по Москве), и тест не должен
	// разъезжаться с ним при следующем таком сдвиге.
	sat := time.Date(2026, 8, 8, statsHour, 0, 0, 0, time.Local)

	cases := []struct {
		name string
		now  time.Time
		want time.Time
	}{
		{"ровно в момент расписания", sat, sat},
		{"суббота, часом позже", sat.Add(time.Hour), sat},
		{"суббота, но час расписания ещё не наступил", sat.Add(-2 * time.Hour), sat.AddDate(0, 0, -7)},
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

// TestGeocodeStats — одна таблица на все вызовы: попадания, походы в сеть и
// ожидания чужого запроса. Главное здесь — что попадания НЕ портят статистику
// ожидания (у них NULL, а не ноль) и что видны ожидания дольше порога.
func TestGeocodeStats(t *testing.T) {
	store := openTestStore(t)
	now := time.Now().UnixMilli()
	dayStart := time.Now().UTC().Truncate(24 * time.Hour).UnixMilli()
	from, to := dayStart, dayStart+24*3600*1000

	req := func(source, country, failure string, wait time.Duration) {
		t.Helper()
		if err := store.SaveGeocodeRequest(GeocodeRequest{
			TS: now, Source: source, Country: country, Err: failure, Wait: wait,
		}); err != nil {
			t.Fatalf("save geocode request: %v", err)
		}
	}
	req(geocodeSourceNet, "RU", "", 2*time.Second)
	req(geocodeSourceNet, "DE", "", 7*time.Second)               // очередь: дольше порога
	req(geocodeSourceNet, "", geocodeErrTimeout, 15*time.Second) // не дождались
	req(geocodeSourceNet, "", "http_429", time.Second)           // притормозили за лимит
	req(geocodeSourceJoined, "RU", "", 6*time.Second)            // ждал чужой запрос
	for i := 0; i < 4; i++ {
		req(geocodeSourceCache, "RU", "", 0)
	}

	// Инвариант схемы: у попаданий ожидание NULL, иначе они утянули бы среднее.
	var nulls int
	if err := store.db.QueryRow(
		`SELECT COUNT(*) FROM geocode_request WHERE wait_ms IS NULL`).Scan(&nulls); err != nil {
		t.Fatalf("count nulls: %v", err)
	}
	if nulls != 4 {
		t.Errorf("строк с NULL-ожиданием %d, want 4 (все попадания)", nulls)
	}
	// Второй инвариант: «получилось» — это NULL, а не пустая строка, иначе
	// COUNT(error) считал бы успехи наравне с отказами.
	var ok int
	if err := store.db.QueryRow(
		`SELECT COUNT(*) FROM geocode_request WHERE error IS NULL`).Scan(&ok); err != nil {
		t.Fatalf("count ok: %v", err)
	}
	if ok != 7 {
		t.Errorf("строк без метки отказа %d, want 7 (2 успешных похода + joined + 4 попадания)", ok)
	}

	st, err := store.WeeklyStats(from, to)
	if err != nil {
		t.Fatalf("weekly stats: %v", err)
	}
	// joined в «сколько раз сходили в сеть» не считается: сети там не было.
	if st.GeocodeRequests != 4 || st.GeocodeErrors != 2 {
		t.Errorf("запросов/отказов = %d/%d, want 4/2", st.GeocodeRequests, st.GeocodeErrors)
	}
	if st.GeocodeCacheHits != 4 {
		t.Errorf("попаданий = %d, want 4", st.GeocodeCacheHits)
	}
	// Причины важнее их суммы: по ним видно, что в лимит мы упёрлись один раз, а
	// не «отказов было два».
	if got := groupCount(st.ByGeocodeError, "http_429"); got != 1 {
		t.Errorf("ByGeocodeError[http_429] = %d, want 1", got)
	}
	if got := groupCount(st.ByGeocodeError, geocodeErrTimeout); got != 1 {
		t.Errorf("ByGeocodeError[timeout] = %d, want 1", got)
	}
	// Успехи в разбивку причин попадать не должны: у них reason пустой.
	if got := groupCount(st.ByGeocodeError, ""); got != 0 {
		t.Errorf("ByGeocodeError[''] = %d, want 0", got)
	}
	// Ожидание — по пяти строкам с ожиданием (2+7+15+1+6 с); четыре попадания в
	// среднее не попали, иначе оно было бы вдвое меньше.
	if st.WaitMaxMs != 15000 {
		t.Errorf("худшее ожидание = %d мс, want 15000", st.WaitMaxMs)
	}
	if st.WaitAvgMs != 6200 {
		t.Errorf("среднее ожидание = %d мс, want 6200 (NULL не участвуют)", st.WaitAvgMs)
	}
	if st.WaitSlowN != 3 {
		t.Errorf("дольше порога = %d, want 3 (7 с, 15 с и joined 6 с)", st.WaitSlowN)
	}
	// Страны — только про походы в сеть, и без пустой: она означает отказ.
	if got := groupCount(st.ByGeocodeCountry, "RU"); got != 1 {
		t.Errorf("ByGeocodeCountry[RU] = %d, want 1 (без joined и кеша)", got)
	}
	if got := groupCount(st.ByGeocodeCountry, ""); got != 0 {
		t.Errorf("ByGeocodeCountry[''] = %d, want 0", got)
	}

	// Соседние сутки в окно не попадают: сводка недельная, но границы честные.
	st, err = store.WeeklyStats(to, to+24*3600*1000)
	if err != nil {
		t.Fatalf("weekly stats (следующие сутки): %v", err)
	}
	if st.GeocodeRequests != 0 || st.GeocodeCacheHits != 0 || st.WaitMaxMs != 0 {
		t.Errorf("следующие сутки не пусты: %+v", st)
	}
}

// Уборщик обязан снимать старые строки: geocode_request растёт быстрее всех наших
// таблиц (строка на каждый locate), и без TTL она заняла бы базу целиком.
func TestGeocodeRequestCleanup(t *testing.T) {
	store := openTestStore(t)
	old := time.Now().Add(-2 * geocodeRequestTTL).UnixMilli()
	fresh := time.Now().UnixMilli()

	for _, ts := range []int64{old, fresh} {
		if err := store.SaveGeocodeRequest(GeocodeRequest{
			TS: ts, Source: geocodeSourceNet, Country: "RU",
		}); err != nil {
			t.Fatalf("save: %v", err)
		}
	}
	n, err := store.DeleteGeocodeRequestsOlderThan(geocodeRequestTTL)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if n != 1 {
		t.Errorf("удалено %d, want 1", n)
	}
	var left int64
	if err := store.db.QueryRow(`SELECT ts FROM geocode_request`).Scan(&left); err != nil {
		t.Fatalf("read: %v", err)
	}
	if left != fresh {
		t.Errorf("осталась строка ts=%d, want свежую %d", left, fresh)
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
	if !strings.Contains(text, "когда | источник | позвал | платформа→куда | клиент") {
		t.Errorf("у списка переходов нет шапки: %q", text)
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
