package main

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// openTestStore — свежая база на тест.
func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// mkTgUser заводит аккаунт тем же путём, что настоящий вход: личность у
// провайдера → внутренний id. Telegram выбран по умолчанию потому, что он был
// единственным провайдером до Apple/Google, и старые проверки читаются как
// раньше; где важна разница между провайдерами, UpsertByIdentity зовётся прямо.
func mkTgUser(t *testing.T, s *Store, tgUID, username, name string) int64 {
	t.Helper()
	id, _, _, err := s.UpsertByIdentity(ProviderUser{
		Provider: ProviderTelegram, UID: tgUID, Username: username, Name: name,
	})
	if err != nil {
		t.Fatalf("создать пользователя %s: %v", tgUID, err)
	}
	return id
}

func TestStoreUsersAndSessions(t *testing.T) {
	s := openTestStore(t)

	pu := ProviderUser{Provider: ProviderTelegram, UID: "42", Username: "alex", Name: "Alex"}
	// первый вход — регистрация: created=true
	id, created, accepted, err := s.UpsertByIdentity(pu)
	if err != nil {
		t.Fatalf("первый вход: %v", err)
	}
	if !created || accepted {
		t.Fatalf("первый вход: created=%v accepted=%v, want true/false", created, accepted)
	}
	// внутренний id — свой, а не id у провайдера: Telegram id наружу не уходит
	if id == 42 {
		t.Fatal("внутренний id совпал с Telegram id — он не должен из базы выходить")
	}

	token, err := s.NewSession(id)
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
	if got == nil || got.ID != id || got.TgUsername != "alex" || got.FullName != "Alex" {
		t.Fatalf("resume: got %+v, want id=%d alex/Alex", got, id)
	}
	// дата регистрации нужна лимитеру: у свежего аккаунта тир уже (ratelimit.go)
	if got.CreatedAt.IsZero() || time.Since(got.CreatedAt) > time.Minute {
		t.Fatalf("resume: CreatedAt = %v, ожидали «только что»", got.CreatedAt)
	}

	// неизвестный токен — не ошибка, а «сессии нет»
	got, err = s.UserBySession("deadbeef")
	if err != nil {
		t.Fatalf("unknown token: %v", err)
	}
	if got != nil {
		t.Fatalf("unknown token: got %+v, want nil", got)
	}

	// повторный вход тем же провайдером — тот же аккаунт, НЕ регистрация
	pu.Name = "alex_new"
	again, created, _, err := s.UpsertByIdentity(pu)
	if err != nil {
		t.Fatalf("повторный вход: %v", err)
	}
	if created {
		t.Fatal("повторный вход посчитан регистрацией")
	}
	if again != id {
		t.Fatalf("повторный вход дал другой id: %d != %d", again, id)
	}
	// ...и имя при этом НЕ меняется: провайдер заполняет его только при
	// регистрации, пока оно пустое. Иначе имя, введённое человеком на экране
	// онбординга, слетало бы при каждом входе через провайдера.
	got, err = s.UserBySession(token)
	if err != nil {
		t.Fatalf("resume after update: %v", err)
	}
	if got == nil || got.FullName != "Alex" {
		t.Fatalf("resume after update: got %+v, want full_name Alex (провайдер не должен перезаписывать имя)", got)
	}

	// правила принимаются один раз и переживают повторные входы
	if err := s.AcceptRules(id); err != nil {
		t.Fatalf("accept rules: %v", err)
	}
	if _, _, accepted, err := s.UpsertByIdentity(pu); err != nil {
		t.Fatalf("вход после accept: %v", err)
	} else if !accepted {
		t.Fatal("вход после accept: rules_accepted = false, want true")
	}
	got, err = s.UserBySession(token)
	if err != nil {
		t.Fatalf("resume after accept: %v", err)
	}
	if got == nil || !got.RulesAccepted {
		t.Fatalf("resume after accept: got %+v, want RulesAccepted=true", got)
	}
}

// Три провайдера в одной базе: аккаунты независимы, а один и тот же UID у разных
// провайдеров — это разные люди (ключ в identities составной).
func TestStoreIdentities(t *testing.T) {
	s := openTestStore(t)

	tg := mkTgUser(t, s, "777", "alex", "Alex")
	apple, created, _, err := s.UpsertByIdentity(ProviderUser{
		Provider: ProviderApple, UID: "000123.abc", Name: "Алексей",
	})
	if err != nil || !created {
		t.Fatalf("вход через Apple: id=%d created=%v err=%v", apple, created, err)
	}
	if apple == tg {
		t.Fatal("Apple-вход попал в тот же аккаунт, что Telegram")
	}
	// один и тот же UID у другого провайдера — другой человек
	same, created, _, err := s.UpsertByIdentity(ProviderUser{
		Provider: ProviderGoogle, UID: "777", Name: "Однофамилец",
	})
	if err != nil || !created || same == tg {
		t.Fatalf("google:777 склеился с telegram:777: id=%d created=%v err=%v", same, created, err)
	}

	// @username и аватар — атрибуты Telegram-входа; у Apple их нет
	u, err := s.UserByID(apple)
	if err != nil || u == nil {
		t.Fatalf("UserByID: %+v err=%v", u, err)
	}
	if u.TgUsername != "" || u.AvatarURL != "" {
		t.Fatalf("у Apple-аккаунта появились @username/аватар: %+v", u)
	}

	ids, err := s.Identities(tg)
	if err != nil || len(ids) != 1 || ids[0].Provider != ProviderTelegram || ids[0].UID != "777" {
		t.Fatalf("Identities: %+v err=%v", ids, err)
	}
}

// Apple отдаёт имя ТОЛЬКО при первой авторизации, а фото не отдаёт вовсе:
// второй вход не должен обнулить сохранённый профиль.
func TestStoreUpsertKeepsProfile(t *testing.T) {
	s := openTestStore(t)

	first := ProviderUser{Provider: ProviderApple, UID: "sub-1", Name: "Мария"}
	id, _, _, err := s.UpsertByIdentity(first)
	if err != nil {
		t.Fatalf("первый вход: %v", err)
	}
	// второй вход — токен без имени
	if _, _, _, err := s.UpsertByIdentity(ProviderUser{Provider: ProviderApple, UID: "sub-1"}); err != nil {
		t.Fatalf("второй вход: %v", err)
	}
	u, err := s.UserByID(id)
	if err != nil || u == nil {
		t.Fatalf("UserByID: %v", err)
	}
	if u.FullName != "Мария" {
		t.Fatalf("имя затёрлось пустым: %q", u.FullName)
	}

	// а вот Telegram-вход @username менять вправе — там пустое значение законно
	tgID := mkTgUser(t, s, "5", "old_nick", "Пётр")
	if _, _, _, err := s.UpsertByIdentity(ProviderUser{
		Provider: ProviderTelegram, UID: "5", Username: "", Name: "Пётр",
	}); err != nil {
		t.Fatalf("Telegram без username: %v", err)
	}
	if u, _ := s.UserByID(tgID); u == nil || u.TgUsername != "" {
		t.Fatalf("@username не очистился при входе без него: %+v", u)
	}
}

func TestStoreMessages(t *testing.T) {
	s := openTestStore(t)

	// автор: ник/@username/аватар в messages не хранятся — History берёт их
	// JOIN из users по user_id
	author, _, _, err := s.UpsertByIdentity(ProviderUser{
		Provider:  ProviderTelegram,
		UID:       "42",
		Username:  "alex_tg",
		Name:      "alex",
		AvatarURL: "https://t.me/i/alex.jpg",
	})
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}

	var ids []int64
	for i, text := range []string{"один", "два", "три", "четыре", "пять"} {
		id, err := s.SaveMessage("RU", author, text, int64(1000+i))
		if err != nil {
			t.Fatalf("save %q: %v", text, err)
		}
		ids = append(ids, id)
	}
	if _, err := s.SaveMessage("DE", author, "hallo", 2000); err != nil {
		t.Fatalf("save DE: %v", err)
	}

	// последняя страница: 3 новейших, хронологически
	msgs, err := s.History("RU", 0, 3)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(msgs) != 3 || msgs[0].Text != "три" || msgs[2].Text != "пять" {
		t.Fatalf("history page 1: %+v", msgs)
	}
	if msgs[0].ID != ids[2] || msgs[0].Channel != "RU" || msgs[0].TS != 1002 {
		t.Fatalf("history fields: %+v", msgs[0])
	}
	// ник и аватар подтянуты JOIN из users
	if msgs[0].Sender != "alex" ||
		msgs[0].SenderID != author ||
		msgs[0].Username != "alex_tg" ||
		msgs[0].AvatarURL != "https://t.me/i/alex.jpg" {
		t.Fatalf("history join users: %+v", msgs[0])
	}

	// страница вверх от начала предыдущей
	msgs, err = s.History("RU", msgs[0].ID, 10)
	if err != nil {
		t.Fatalf("history before: %v", err)
	}
	if len(msgs) != 2 || msgs[0].Text != "один" || msgs[1].Text != "два" {
		t.Fatalf("history page 2: %+v", msgs)
	}

	// чужой канал не подмешивается, пустой — пустой список
	msgs, err = s.History("FR", 0, 10)
	if err != nil {
		t.Fatalf("history empty: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("history empty: %+v", msgs)
	}
}

func TestStoreDeleteUser(t *testing.T) {
	s := openTestStore(t)

	id := mkTgUser(t, s, "7", "alex", "Alex")
	// два устройства — две сессии
	t1, err := s.NewSession(id)
	if err != nil {
		t.Fatalf("session 1: %v", err)
	}
	t2, err := s.NewSession(id)
	if err != nil {
		t.Fatalf("session 2: %v", err)
	}
	if _, err := s.SaveMessage("RU", id, "привет", 1000); err != nil {
		t.Fatalf("save message: %v", err)
	}

	if err := s.DeleteUser(id); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// все сессии пользователя мертвы (удаление аккаунта — со всех устройств)
	for _, tok := range []string{t1, t2} {
		got, err := s.UserBySession(tok)
		if err != nil {
			t.Fatalf("resume after delete: %v", err)
		}
		if got != nil {
			t.Fatalf("resume after delete: сессия %s жива: %+v", tok, got)
		}
	}

	// сообщения удалены каскадом вместе с аккаунтом (ON DELETE CASCADE)
	msgs, err := s.History("RU", 0, 10)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("history: got %d messages, want 0 (удалены вместе с аккаунтом)", len(msgs))
	}

	// повторное удаление — не ошибка (идемпотентно)
	if err := s.DeleteUser(id); err != nil {
		t.Fatalf("delete again: %v", err)
	}
	// способ входа ушёл вместе с аккаунтом — иначе он бы вечно занимал
	// провайдерский uid и вернувшийся не смог бы зарегистрироваться заново
	if ids, err := s.Identities(id); err != nil || len(ids) != 0 {
		t.Fatalf("после удаления остались личности: %+v err=%v", ids, err)
	}
}

func TestStoreDeleteMessagesOlderThan(t *testing.T) {
	s := openTestStore(t)

	author := mkTgUser(t, s, "42", "", "alex")

	ttl := 7 * 24 * time.Hour
	now := time.Now()
	// два сообщения по краям TTL: одно чуть старше границы, одно свежее
	stale, err := s.SaveMessage("RU", author, "старое", now.Add(-ttl-time.Hour).UnixMilli())
	if err != nil {
		t.Fatalf("save stale: %v", err)
	}
	if _, err := s.SaveMessage("RU", author, "свежее", now.Add(-time.Hour).UnixMilli()); err != nil {
		t.Fatalf("save fresh: %v", err)
	}

	n, err := s.DeleteMessagesOlderThan(ttl)
	if err != nil {
		t.Fatalf("delete old: %v", err)
	}
	if n != 1 {
		t.Fatalf("удалено %d сообщений, want 1", n)
	}

	msgs, err := s.History("RU", 0, 10)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Text != "свежее" {
		t.Fatalf("после уборки осталось %+v, want только «свежее»", msgs)
	}
	if msgs[0].ID == stale {
		t.Fatal("осталось именно старое сообщение")
	}

	// повторный проход больше нечего удалять
	if n, err := s.DeleteMessagesOlderThan(ttl); err != nil || n != 0 {
		t.Fatalf("повторная уборка: n=%d err=%v, want 0 nil", n, err)
	}
}

func TestStoreReportMessage(t *testing.T) {
	s := openTestStore(t)

	author := mkTgUser(t, s, "1", "", "Author")
	reporter := mkTgUser(t, s, "2", "", "Reporter")
	msgID, err := s.SaveMessage("RU", author, "гадость", 1000)
	if err != nil {
		t.Fatalf("save message: %v", err)
	}

	rep, err := s.ReportMessage(msgID, reporter, "abuse")
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if rep == nil {
		t.Fatal("report: nil для существующего сообщения")
	}
	if !rep.Fresh {
		t.Fatal("report: Fresh = false для первой жалобы")
	}
	// данные для уведомления в канал модерации собраны из сообщения и автора
	if rep.Text != "гадость" || rep.AuthorID != author || rep.AuthorName != "Author" {
		t.Fatalf("report: %+v", rep)
	}

	// текст и автор скопированы — жалоба разбираема после удаления сообщения
	var gotText, gotReason string
	var gotAuthor int64
	if err := s.db.QueryRow(
		`SELECT message_text, author_user_id, reason FROM reports WHERE message_id = ?`, msgID).
		Scan(&gotText, &gotAuthor, &gotReason); err != nil {
		t.Fatalf("select report: %v", err)
	}
	if gotText != "гадость" || gotAuthor != author || gotReason != "abuse" {
		t.Fatalf("report: text=%q author=%d reason=%q", gotText, gotAuthor, gotReason)
	}

	// повторная жалоба того же пользователя — успех, но без дубля; Fresh=false,
	// чтобы не постить в канал модерации второй раз
	if again, err := s.ReportMessage(msgID, reporter, "spam"); err != nil || again == nil {
		t.Fatalf("report again: rep=%v err=%v", again, err)
	} else if again.Fresh {
		t.Fatal("report again: Fresh = true для повторной жалобы")
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM reports WHERE message_id = ?`, msgID).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("report дублируется: %d записей", count)
	}

	// жалоба на несуществующее сообщение — nil, но не ошибка
	if missing, err := s.ReportMessage(9999, reporter, "spam"); err != nil || missing != nil {
		t.Fatalf("report missing: rep=%v err=%v", missing, err)
	}

	// жалоба переживает удаление самого сообщения (TTL-уборка)
	if _, err := s.DeleteMessagesOlderThan(0); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM reports`).Scan(&count); err != nil {
		t.Fatalf("count after cleanup: %v", err)
	}
	if count != 1 {
		t.Fatalf("жалоба пропала после удаления сообщения: %d", count)
	}
}

func TestStorePushTargets(t *testing.T) {
	s := openTestStore(t)

	// автор (1), сосед по каналу с двумя устройствами (2), человек из другого
	// района (3) — пуш должен уйти только устройствам второго
	author := mkTgUser(t, s, "1", "", "Author")
	neighbour := mkTgUser(t, s, "2", "", "Neighbour")
	far := mkTgUser(t, s, "3", "", "Far")
	_ = author
	if err := s.SetUserChannels(author, []string{"RU", "RU-MOW", "relation/1"}); err != nil {
		t.Fatalf("channels 1: %v", err)
	}
	if err := s.SetUserChannels(neighbour, []string{"RU", "RU-MOW", "relation/1"}); err != nil {
		t.Fatalf("channels 2: %v", err)
	}
	if err := s.SetUserChannels(far, []string{"RU", "relation/999"}); err != nil {
		t.Fatalf("channels 3: %v", err)
	}
	for _, d := range []struct {
		user int64
		tok  string
	}{
		{author, "dev-author"}, {neighbour, "dev-n1"}, {neighbour, "dev-n2"}, {far, "dev-far"},
	} {
		if err := s.SaveDeviceToken(d.user, d.tok, "android"); err != nil {
			t.Fatalf("token %s: %v", d.tok, err)
		}
	}

	// автор исключён — это и есть суть адресной модели (в топик так было нельзя)
	got, err := s.PushTargets("relation/1", author)
	if err != nil {
		t.Fatalf("targets: %v", err)
	}
	want := map[string]bool{"dev-n1": true, "dev-n2": true}
	if len(got) != len(want) {
		t.Fatalf("targets: %v, want %v", got, want)
	}
	for _, tok := range got {
		if !want[tok] {
			t.Fatalf("лишний получатель %q (автор или чужой район): %v", tok, got)
		}
	}

	// переезд: набор каналов заменяется целиком, из старого района пуши больше
	// не приходят
	if err := s.SetUserChannels(neighbour, []string{"RU", "relation/777"}); err != nil {
		t.Fatalf("move: %v", err)
	}
	if got, err := s.PushTargets("relation/1", author); err != nil || len(got) != 0 {
		t.Fatalf("после переезда targets=%v err=%v, want пусто", got, err)
	}

	// токен переезжает между аккаунтами (переустановка/смена аккаунта), не дублируясь
	if err := s.SaveDeviceToken(far, "dev-n1", "ios"); err != nil {
		t.Fatalf("re-bind: %v", err)
	}
	got, err = s.PushTargets("relation/999", author)
	if err != nil {
		t.Fatalf("targets after re-bind: %v", err)
	}
	if len(got) != 2 { // dev-far + переехавший dev-n1
		t.Fatalf("targets after re-bind: %v, want 2", got)
	}

	// мёртвые токены убираются (FCM ответил UNREGISTERED)
	if err := s.DeleteDeviceTokens([]string{"dev-far", "dev-n1"}); err != nil {
		t.Fatalf("delete stale: %v", err)
	}
	if got, err := s.PushTargets("relation/999", author); err != nil || len(got) != 0 {
		t.Fatalf("после уборки targets=%v err=%v, want пусто", got, err)
	}

	// удаление аккаунта уносит его токены и каналы (ON DELETE CASCADE)
	if err := s.SaveDeviceToken(neighbour, "dev-n3", "android"); err != nil {
		t.Fatalf("token: %v", err)
	}
	if err := s.DeleteUser(neighbour); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM device_tokens WHERE user_id = ?`, neighbour).Scan(&n); err != nil {
		t.Fatalf("count tokens: %v", err)
	}
	if n != 0 {
		t.Fatalf("после удаления аккаунта осталось %d токенов", n)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM user_channels WHERE user_id = ?`, neighbour).Scan(&n); err != nil {
		t.Fatalf("count channels: %v", err)
	}
	if n != 0 {
		t.Fatalf("после удаления аккаунта осталось %d каналов", n)
	}
}

// Наказания модератора: мьют на сутки и постоянный бан — две независимые
// операции, автоматической эскалации нет (решает модератор, см. admin.go).
// Главное здесь — наказание переживает удаление аккаунта: оно кладётся на
// ЛИЧНОСТЬ у провайдера, а не на внутренний id, иначе нарушитель удалил бы
// аккаунт и вошёл тем же Telegram/Apple с чистого листа.
func TestStoreBans(t *testing.T) {
	s := openTestStore(t)

	const tgUID = "99"
	id := mkTgUser(t, s, tgUID, "", "Нарушитель")
	token, err := s.NewSession(id)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	if _, err := s.SaveMessage("RU", id, "гадость", time.Now().UnixMilli()); err != nil {
		t.Fatalf("save message: %v", err)
	}

	// чистый аккаунт не наказан
	if banned, _, _, _, err := s.BanStatus(id); err != nil || banned {
		t.Fatalf("до бана: banned=%v err=%v", banned, err)
	}

	// мьют на сутки, с причиной
	until, count, err := s.BanTemporary(id, "реклама")
	if err != nil {
		t.Fatalf("ban: %v", err)
	}
	if count != 1 {
		t.Fatalf("счётчик нарушений = %d, want 1", count)
	}
	if d := time.Until(until); d < banDuration-time.Minute || d > banDuration+time.Minute {
		t.Fatalf("срок мьюта %v, ожидали ~%v", d, banDuration)
	}
	banned, gotUntil, permanent, reason, err := s.BanStatus(id)
	if err != nil || !banned || permanent || gotUntil.IsZero() || reason != "реклама" {
		t.Fatalf("после мьюта: banned=%v perm=%v until=%v reason=%q err=%v",
			banned, permanent, gotUntil, reason, err)
	}
	// Сессия ЖИВА: мьют — не выселение. Человек продолжает читать, гасится
	// только отправка (см. publish в client.go).
	u, err := s.UserBySession(token)
	if err != nil || u == nil {
		t.Fatalf("сессия после мьюта: %+v err=%v, want живую", u, err)
	}
	if !u.Banned || u.BanPermanent {
		t.Fatalf("после мьюта: Banned=%v BanPermanent=%v, want true/false", u.Banned, u.BanPermanent)
	}
	// контент мьют не трогает
	if msgs, err := s.History("RU", 0, 10); err != nil || len(msgs) != 1 {
		t.Fatalf("мьют не должен удалять сообщения: %v err=%v", msgs, err)
	}

	// повторный мьют не превращается в постоянный сам — только счётчик растёт
	if _, count, err = s.BanTemporary(id, ""); err != nil || count != 2 {
		t.Fatalf("второй мьют: count=%d err=%v, want 2", count, err)
	}
	if _, _, permanent, _, _ := s.BanStatus(id); permanent {
		t.Fatal("второй мьют стал постоянным — автоэскалации быть не должно")
	}

	// постоянный бан — отдельной командой; аккаунт и его сообщения удаляются
	count, err = s.BanPermanent(id, "спам")
	if err != nil {
		t.Fatalf("block: %v", err)
	}
	if count != 3 {
		t.Fatalf("счётчик после block = %d, want 3", count)
	}
	if msgs, err := s.History("RU", 0, 10); err != nil || len(msgs) != 0 {
		t.Fatalf("после блокировки сообщения остались: %v err=%v", msgs, err)
	}
	var users int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM users WHERE id = ?`, id).Scan(&users); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if users != 0 {
		t.Fatal("аккаунт не удалён при постоянном бане")
	}
	// наказывать удалённого больше нечем — модератору отвечаем внятно, а не
	// «смотри логи» (см. admin.go)
	if _, _, err := s.BanTemporary(id, ""); !errors.Is(err, ErrNoIdentities) {
		t.Fatalf("ban удалённого: err=%v, want ErrNoIdentities", err)
	}

	// САМОЕ ВАЖНОЕ: наказание живо, хотя аккаунта уже нет. Проверяется по
	// личности — именно её предъявляет вернувшийся при входе (handleAuth),
	// внутреннего id у него в этот момент ещё нет.
	banned, _, permanent, _, err = s.BanStatusForIdentity(ProviderTelegram, tgUID)
	if err != nil || !banned || !permanent {
		t.Fatalf("после удаления аккаунта: banned=%v perm=%v err=%v (наказание должно переживать удаление)",
			banned, permanent, err)
	}

	// повторная регистрация тем же Telegram-аккаунтом: внутренний id НОВЫЙ,
	// но наказание и счётчик нарушений подтягиваются к нему через личность —
	// иначе рецидивист выглядел бы новичком
	again := mkTgUser(t, s, tgUID, "", "Он же снова")
	if again == id {
		t.Fatal("повторная регистрация переиспользовала внутренний id")
	}
	if banned, _, permanent, _, _ := s.BanStatus(again); !banned || !permanent {
		t.Fatal("после повторной регистрации наказание слетело")
	}
	if n, err := s.BanCount(again); err != nil || n != 3 {
		t.Fatalf("счётчик нарушений у вернувшегося = %d err=%v, want 3", n, err)
	}

	// ручное снятие возвращает доступ, но счётчик нарушений остаётся —
	// модератор должен видеть историю
	if ok, err := s.Unban(again); err != nil || !ok {
		t.Fatalf("unban: ok=%v err=%v", ok, err)
	}
	if banned, _, _, _, _ := s.BanStatus(again); banned {
		t.Fatal("после unban всё ещё забанен")
	}
	if n, err := s.BanCount(again); err != nil || n != 3 {
		t.Fatalf("счётчик после unban = %d err=%v, want 3", n, err)
	}
}

// Бан на одном провайдере закрывает вход и через второй: наказание пишется на
// все способы входа человека, иначе оно обходилось бы сменой кнопки на экране входа.
func TestStoreBanCoversAllProviders(t *testing.T) {
	s := openTestStore(t)

	id := mkTgUser(t, s, "500", "", "Двуликий")
	// тот же аккаунт получил вторую личность (привязки в UI пока нет —
	// эмулируем то, что схема уже допускает)
	if _, err := s.db.Exec(`
		INSERT INTO identities (provider, provider_uid, user_id, created_at) VALUES (?, ?, ?, 0)`,
		ProviderApple, "sub-500", id); err != nil {
		t.Fatalf("привязать Apple: %v", err)
	}

	if _, _, err := s.BanTemporary(id, "спам"); err != nil {
		t.Fatalf("ban: %v", err)
	}
	for _, p := range []struct{ provider, uid string }{
		{ProviderTelegram, "500"}, {ProviderApple, "sub-500"},
	} {
		banned, _, _, _, err := s.BanStatusForIdentity(p.provider, p.uid)
		if err != nil || !banned {
			t.Fatalf("%s: banned=%v err=%v, want забанен", p.provider, banned, err)
		}
	}
}

// Временный бан истекает сам, без фоновой уборки.
func TestStoreBanExpires(t *testing.T) {
	s := openTestStore(t)

	id := mkTgUser(t, s, "100", "", "x")
	if _, _, err := s.BanTemporary(id, ""); err != nil {
		t.Fatalf("ban: %v", err)
	}
	// сдвигаем срок в прошлое — эмулируем «сутки прошли»
	if _, err := s.db.Exec(`UPDATE bans SET until = ? WHERE user_id = ?`,
		time.Now().Add(-time.Minute).UnixMilli(), id); err != nil {
		t.Fatalf("expire: %v", err)
	}
	if banned, _, _, _, err := s.BanStatus(id); err != nil || banned {
		t.Fatalf("истёкший мьют всё ещё активен: banned=%v err=%v", banned, err)
	}
}

func TestStoreChannelSubscribers(t *testing.T) {
	s := openTestStore(t)

	a := mkTgUser(t, s, "1", "", "a")
	b := mkTgUser(t, s, "2", "", "b")
	c := mkTgUser(t, s, "3", "", "c")
	// трое в стране, двое в одном районе, третий в своём
	if err := s.SetUserChannels(a, []string{"RU", "relation/1"}); err != nil {
		t.Fatalf("channels 1: %v", err)
	}
	if err := s.SetUserChannels(b, []string{"RU", "relation/1"}); err != nil {
		t.Fatalf("channels 2: %v", err)
	}
	if err := s.SetUserChannels(c, []string{"RU", "relation/2"}); err != nil {
		t.Fatalf("channels 3: %v", err)
	}

	got, err := s.ChannelSubscribers([]string{"EARTH", "RU", "relation/1", "relation/2"})
	if err != nil {
		t.Fatalf("subscribers: %v", err)
	}
	// EARTH никто ещё не локейтил — канала в карте нет, вызывающий читает 0
	for ch, want := range map[string]int{"EARTH": 0, "RU": 3, "relation/1": 2, "relation/2": 1} {
		if got[ch] != want {
			t.Fatalf("%s: %d, want %d (all=%v)", ch, got[ch], want, got)
		}
	}

	// пустой список — не паника и не SQL с болтающимся IN ()
	if m, err := s.ChannelSubscribers(nil); err != nil || len(m) != 0 {
		t.Fatalf("пустой список: %v %v", m, err)
	}

	// удаление аккаунта уносит его каналы (ON DELETE CASCADE) — счётчик падает
	if err := s.DeleteUser(b); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if m, err := s.ChannelSubscribers([]string{"relation/1"}); err != nil || m["relation/1"] != 1 {
		t.Fatalf("после удаления аккаунта relation/1=%d err=%v, want 1", m["relation/1"], err)
	}
}
