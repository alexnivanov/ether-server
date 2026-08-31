package main

import (
	"errors"
	"net/http"
	"testing"
	"time"
)

// mkMessage — сообщение автора в канале, чтобы за него было чем голосовать.
func mkMessage(t *testing.T, s *Store, channel string, author int64, text string) int64 {
	t.Helper()
	id, _, err := s.SaveMessage(channel, author, text, time.Now().UnixMilli(), "")
	if err != nil {
		t.Fatalf("сохранить сообщение: %v", err)
	}
	return id
}

// Голос за сообщение, эффект на автора: рейтинг это простая сумма голосов, и
// каждый голос стоит ровно 1 — никаких потолков на пару.
func TestVoteRatingIsPlainSum(t *testing.T) {
	s := openTestStore(t)
	author := mkTgUser(t, s, "1", "author", "Автор")
	fan := mkTgUser(t, s, "2", "fan", "Читатель")
	other := mkTgUser(t, s, "3", "other", "Другой")

	a := mkMessage(t, s, "RU", author, "первое")
	b := mkMessage(t, s, "RU", author, "второе")

	// один человек отметил два сообщения — оба голоса в рейтинге
	if _, err := s.Vote(fan, a, 1); err != nil {
		t.Fatalf("голос: %v", err)
	}
	if _, err := s.Vote(fan, b, 1); err != nil {
		t.Fatalf("голос: %v", err)
	}
	if r, err := s.AuthorRating(author); err != nil || r != 2 {
		t.Fatalf("рейтинг = %d (err %v), want 2", r, err)
	}
	// минус стоит столько же, сколько плюс
	if _, err := s.Vote(other, a, -1); err != nil {
		t.Fatalf("минус: %v", err)
	}
	if r, _ := s.AuthorRating(author); r != 1 {
		t.Fatalf("рейтинг после минуса = %d, want 1", r)
	}
	// голосующим рейтинг не начисляется
	if r, _ := s.AuthorRating(fan); r != 0 {
		t.Fatalf("голосующий получил рейтинг: %d", r)
	}
}

// Запас общий на всех авторов и ограничивает само голосование. Отказ приходит
// на попытке, а не молча обесценивает нажатие.
func TestVoteBudgetIsShared(t *testing.T) {
	s := openTestStore(t)
	voter := mkTgUser(t, s, "1", "voter", "Голосующий")

	var last int64
	for i := 0; i < voteBudget; i++ {
		// разные авторы: запас общий, и от того, кому отдан голос, он не зависит
		author := mkTgUser(t, s, string(rune('a'+i)), "", "Автор")
		last = mkMessage(t, s, "RU", author, "текст")
		st, err := s.Vote(voter, last, 1)
		if err != nil {
			t.Fatalf("голос %d: %v", i, err)
		}
		if want := voteBudget - i - 1; st.VotesLeft != want {
			t.Fatalf("после голоса %d остаток %d, want %d", i, st.VotesLeft, want)
		}
	}
	extra := mkMessage(t, s, "RU", mkTgUser(t, s, "z", "", "Ещё"), "текст")
	if _, err := s.Vote(voter, extra, 1); !errors.Is(err, ErrNoVotesLeft) {
		t.Fatalf("запас не кончился: %v", err)
	}
	// исчерпанный запас не мешает менять уже отданный голос: слот тот же
	if st, err := s.Vote(voter, last, -1); err != nil || st.MyVote != -1 {
		t.Fatalf("смена знака при пустом запасе: %+v (err %v)", st, err)
	}
	if left, _ := s.VotesLeft(voter); left != 0 {
		t.Fatalf("смена знака истратила запас: остаток %d", left)
	}
}

// Снятие голоса возвращает его в запас сразу: случайное нажатие не должно
// наказывать на неделю.
func TestVoteRetractFreesSlot(t *testing.T) {
	s := openTestStore(t)
	author := mkTgUser(t, s, "1", "author", "Автор")
	voter := mkTgUser(t, s, "2", "voter", "Голосующий")
	m := mkMessage(t, s, "RU", author, "текст")

	if _, err := s.Vote(voter, m, 1); err != nil {
		t.Fatalf("голос: %v", err)
	}
	st, err := s.Vote(voter, m, 0)
	if err != nil {
		t.Fatalf("снятие: %v", err)
	}
	if st.MyVote != 0 || st.Rating != 0 || st.VotesLeft != voteBudget {
		t.Fatalf("снятие не вернуло состояние: %+v", st)
	}
	if r, _ := s.AuthorRating(author); r != 0 {
		t.Fatalf("рейтинг после снятия = %d, want 0", r)
	}
	// снятие несуществующего голоса — тоже успех: клиент мог не получить ответ
	if _, err := s.Vote(voter, m, 0); err != nil {
		t.Fatalf("повторное снятие: %v", err)
	}
}

// Повтор тем же знаком идемпотентен: ни второй строки, ни второго списания.
func TestVoteRepeatIsIdempotent(t *testing.T) {
	s := openTestStore(t)
	author := mkTgUser(t, s, "1", "author", "Автор")
	voter := mkTgUser(t, s, "2", "voter", "Голосующий")
	m := mkMessage(t, s, "RU", author, "текст")

	first, err := s.Vote(voter, m, 1)
	if err != nil {
		t.Fatalf("голос: %v", err)
	}
	second, err := s.Vote(voter, m, 1)
	if err != nil {
		t.Fatalf("повтор: %v", err)
	}
	if first != second {
		t.Fatalf("повтор изменил состояние: %+v → %+v", first, second)
	}
	if r, _ := s.AuthorRating(author); r != 1 {
		t.Fatalf("повтор удвоил рейтинг: %d", r)
	}
}

// За свои сообщения голосовать нельзя — иначе рейтинг набирается в одиночку.
// Голос за уехавшее сообщение — not_found, а не молчаливый успех.
func TestVoteRejects(t *testing.T) {
	s := openTestStore(t)
	author := mkTgUser(t, s, "1", "author", "Автор")
	m := mkMessage(t, s, "RU", author, "текст")

	if _, err := s.Vote(author, m, 1); !errors.Is(err, ErrSelfVote) {
		t.Fatalf("голос за себя прошёл: %v", err)
	}
	if _, err := s.Vote(author, m+1000, 1); !errors.Is(err, ErrMessageGone) {
		t.Fatalf("голос за несуществующее сообщение: %v", err)
	}
}

// Голоса живут ровно столько, сколько отмеченные сообщения: уборка по TTL
// уносит их каскадом и тем самым возвращает людям запас. Отдельного механизма
// восстановления нет и не нужно.
func TestVotesExpireWithMessages(t *testing.T) {
	s := openTestStore(t)
	author := mkTgUser(t, s, "1", "author", "Автор")
	voter := mkTgUser(t, s, "2", "voter", "Голосующий")

	old, _, err := s.SaveMessage("RU", author, "старое",
		time.Now().Add(-messageTTL-time.Hour).UnixMilli(), "")
	if err != nil {
		t.Fatalf("сохранить старое: %v", err)
	}
	fresh := mkMessage(t, s, "RU", author, "свежее")
	if _, err := s.Vote(voter, old, 1); err != nil {
		t.Fatalf("голос за старое: %v", err)
	}
	if _, err := s.Vote(voter, fresh, 1); err != nil {
		t.Fatalf("голос за свежее: %v", err)
	}
	if r, _ := s.AuthorRating(author); r != 2 {
		t.Fatalf("рейтинг до уборки = %d, want 2", r)
	}

	if n, err := s.DeleteMessagesOlderThan(messageTTL); err != nil || n != 1 {
		t.Fatalf("уборка удалила %d сообщений (err %v), want 1", n, err)
	}
	if r, _ := s.AuthorRating(author); r != 1 {
		t.Fatalf("рейтинг после уборки = %d, want 1 — голос уехал с сообщением", r)
	}
	if left, _ := s.VotesLeft(voter); left != voteBudget-1 {
		t.Fatalf("остаток после уборки = %d, want %d", left, voteBudget-1)
	}
}

// История отдаёт сумму под сообщением и свой голос смотрящего. Своё нужно
// отдельно от суммы: плюс мог быть погашен чужим минусом, но кнопка у человека
// всё равно должна выглядеть нажатой.
func TestHistoryCarriesVotes(t *testing.T) {
	s := openTestStore(t)
	author := mkTgUser(t, s, "1", "author", "Автор")
	viewer := mkTgUser(t, s, "2", "viewer", "Смотрящий")
	other := mkTgUser(t, s, "3", "other", "Другой")
	m := mkMessage(t, s, "RU", author, "текст")

	if _, err := s.Vote(viewer, m, 1); err != nil {
		t.Fatalf("плюс: %v", err)
	}
	if _, err := s.Vote(other, m, -1); err != nil {
		t.Fatalf("минус: %v", err)
	}

	msgs, err := s.History("RU", 0, 10, viewer)
	if err != nil {
		t.Fatalf("история: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("сообщений в истории: %d", len(msgs))
	}
	if msgs[0].Rating != 0 {
		t.Fatalf("сумма = %d, want 0 (плюс и минус погасились)", msgs[0].Rating)
	}
	if msgs[0].MyVote != 1 {
		t.Fatalf("свой голос = %d, want 1", msgs[0].MyVote)
	}
	// не вошедший видит сумму, но не «свой» голос
	anon, err := s.History("RU", 0, 10, 0)
	if err != nil {
		t.Fatalf("история без токена: %v", err)
	}
	if anon[0].MyVote != 0 {
		t.Fatalf("у анонима есть свой голос: %d", anon[0].MyVote)
	}
}

// Удаление аккаунта уносит и отданные им голоса: чужие рейтинги, собранные с
// удалённого аккаунта, не остаются висеть.
func TestVotesGoWithVoter(t *testing.T) {
	s := openTestStore(t)
	author := mkTgUser(t, s, "1", "author", "Автор")
	voter := mkTgUser(t, s, "2", "voter", "Голосующий")
	m := mkMessage(t, s, "RU", author, "текст")

	if _, err := s.Vote(voter, m, 1); err != nil {
		t.Fatalf("голос: %v", err)
	}
	if err := s.DeleteUser(voter); err != nil {
		t.Fatalf("удалить голосующего: %v", err)
	}
	if r, _ := s.AuthorRating(author); r != 0 {
		t.Fatalf("рейтинг после удаления голосующего = %d, want 0", r)
	}
}

// Полоса канала берётся из справочника, а не из формы ID: город неотличим от
// района по виду строки, но в channels.level он есть. Район с кварталом
// склеиваются в одну полосу намеренно — метка quarter относительная.
func TestScopeFromChannelsTable(t *testing.T) {
	s := openTestStore(t)
	pub := &publisher{store: s}

	city := "relation/2555133"
	district := "relation/1320555"
	if err := s.SaveChannels([]Channel{
		{ID: city, Level: "city", Name: "Москва"},
		{ID: district, Level: "district", Name: "Тверской"},
		{ID: "relation/9", Level: "quarter", Name: "Патриаршие"},
		{ID: "RU-MOW", Level: "region", Name: "Москва"},
	}); err != nil {
		t.Fatalf("справочник: %v", err)
	}

	cases := []struct {
		channel string
		want    limitScope
	}{
		{city, scopeCity},          // по форме был бы scopeLocal
		{"RU-MOW", scopeCity},      // область едет с городом
		{district, scopeLocal},     // район
		{"relation/9", scopeLocal}, // квартал — та же полоса, что район
		{"relation/404", scopeLocal},
	}
	for _, c := range cases {
		if got := pub.scopeFor(c.channel); got != c.want {
			t.Errorf("scopeFor(%q) = %d, want %d", c.channel, got, c.want)
		}
	}

	// Канала в справочнике нет — падаем на разбор формы, а не отказываем: строки
	// не будет у баз, живших до появления таблицы, и до первого locate в месте.
	if got := pub.scopeFor("EARTH"); got != scopePlanet {
		t.Errorf("EARTH без справочника = %d, want scopePlanet", got)
	}
	if got := pub.scopeFor("DE"); got != scopeCountry {
		t.Errorf("DE без справочника = %d, want scopeCountry", got)
	}
}

// Коды отказов POST /vote — часть контракта: клиент по ним различает «нет
// голосов» (объяснить и не гасить кнопку) от «нельзя» и «сообщение уехало».
func TestVoteEndpointCodes(t *testing.T) {
	srv, store := newTestServer(t)
	voterID, token := publishSession(t, store)
	author := mkTgUser(t, store, "999", "author", "Автор")
	m := mkMessage(t, store, "RU", author, "текст")

	resp, body := restPostAuth(t, srv.URL+"/vote", token, VoteData{MessageID: m, Value: 1})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("голос: %d (%v)", resp.StatusCode, body)
	}
	if body["rating"] != float64(1) || body["my_vote"] != float64(1) {
		t.Errorf("ответ = %v, want rating 1, my_vote 1", body)
	}
	if body["votes_left"] != float64(voteBudget-1) {
		t.Errorf("остаток = %v, want %d", body["votes_left"], voteBudget-1)
	}

	// своё сообщение
	own := mkMessage(t, store, "RU", voterID, "своё")
	resp, body = restPostAuth(t, srv.URL+"/vote", token, VoteData{MessageID: own, Value: 1})
	if resp.StatusCode != http.StatusForbidden || body["code"] != "forbidden" {
		t.Errorf("голос за своё = %d %v, want 403 forbidden", resp.StatusCode, body)
	}

	// уехавшее сообщение
	resp, body = restPostAuth(t, srv.URL+"/vote", token, VoteData{MessageID: m + 1000, Value: 1})
	if resp.StatusCode != http.StatusNotFound || body["code"] != "not_found" {
		t.Errorf("голос за пропавшее = %d %v, want 404 not_found", resp.StatusCode, body)
	}

	// мусор в значении
	resp, _ = restPostAuth(t, srv.URL+"/vote", token, VoteData{MessageID: m, Value: 7})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("value=7 = %d, want 400", resp.StatusCode)
	}

	// без токена — 400 bad_data, с негодным — 401 bad_session (как у /report)
	resp, body = restPostAuth(t, srv.URL+"/vote", "", VoteData{MessageID: m, Value: 1})
	if resp.StatusCode != http.StatusBadRequest || body["code"] != "bad_data" {
		t.Errorf("без токена = %d %v, want 400 bad_data", resp.StatusCode, body)
	}
	resp, body = restPostAuth(t, srv.URL+"/vote", "нет-такого", VoteData{MessageID: m, Value: 1})
	if resp.StatusCode != http.StatusUnauthorized || body["code"] != "bad_session" {
		t.Errorf("с негодным токеном = %d %v, want 401 bad_session", resp.StatusCode, body)
	}

	// исчерпанный запас — 429 с кодом no_votes (не too_fast: у клиента на
	// too_fast заведена ветка «подожди N с», а голоса возвращаются не по таймеру)
	for i := 1; i < voteBudget; i++ {
		extra := mkMessage(t, store, "RU", author, "ещё")
		if resp, body := restPostAuth(t, srv.URL+"/vote", token,
			VoteData{MessageID: extra, Value: 1}); resp.StatusCode != http.StatusOK {
			t.Fatalf("голос %d: %d (%v)", i, resp.StatusCode, body)
		}
	}
	last := mkMessage(t, store, "RU", author, "последнее")
	resp, body = restPostAuth(t, srv.URL+"/vote", token, VoteData{MessageID: last, Value: 1})
	if resp.StatusCode != http.StatusTooManyRequests || body["code"] != "no_votes" {
		t.Errorf("запас кончился = %d %v, want 429 no_votes", resp.StatusCode, body)
	}
}
