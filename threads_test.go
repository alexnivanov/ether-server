package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

// mkReply — ответ в тред корня rootID в канале корня.
func mkReply(t *testing.T, s *Store, rootID int64, channel string, author int64, text string, ts int64) int64 {
	t.Helper()
	id, _, err := s.SaveReply(rootID, channel, author, text, ts, "")
	if err != nil {
		t.Fatalf("сохранить ответ: %v", err)
	}
	return id
}

func exists(t *testing.T, s *Store, id int64) bool {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE id = ?`, id).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n > 0
}

// Лента канала — только корни, у корня счётчик и время последнего ответа;
// тред отдаёт корень и ответы по порядку. Ответы заблокированных не видны и
// не считаются: иначе у корня «2 ответа», а в треде один.
func TestStoreThreads(t *testing.T) {
	s := openTestStore(t)
	a := mkTgUser(t, s, "1", "", "Аня")
	b := mkTgUser(t, s, "2", "", "Боря")
	viewer := mkTgUser(t, s, "3", "", "Вера")

	root := mkMessage(t, s, "RU", a, "корень")
	r1 := mkReply(t, s, root, "RU", b, "ответ Бори", 5000)
	r2 := mkReply(t, s, root, "RU", a, "ответ Ани", 6000)
	plain := mkMessage(t, s, "RU", b, "без ответов")

	feed, err := s.History("RU", 0, 50, 0)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if got := idsOf(feed); !slices.Equal(got, []int64{root, plain}) {
		t.Fatalf("лента = %v, want только корни %v", got, []int64{root, plain})
	}
	if feed[0].ReplyCount != 2 || feed[0].LastReplyTS != 6000 {
		t.Errorf("корень: reply_count=%d last_reply_ts=%d, want 2/6000", feed[0].ReplyCount, feed[0].LastReplyTS)
	}
	if feed[1].ReplyCount != 0 || feed[1].LastReplyTS != 0 {
		t.Errorf("у корня без ответов счётчик: %+v", feed[1])
	}

	gotRoot, replies, err := s.Replies(root, 0)
	if err != nil {
		t.Fatalf("replies: %v", err)
	}
	if gotRoot.ID != root || gotRoot.Text != "корень" {
		t.Errorf("корень треда = %+v", gotRoot)
	}
	if got := idsOf(replies); !slices.Equal(got, []int64{r1, r2}) {
		t.Fatalf("ответы = %v, want %v", got, []int64{r1, r2})
	}
	if replies[0].ParentID != root || replies[0].Sender != "Боря" {
		t.Errorf("ответ = %+v, want parent_id корня и имя автора", replies[0])
	}

	if err := s.BlockUser(viewer, b); err != nil {
		t.Fatalf("block: %v", err)
	}
	feed, _ = s.History("RU", 0, 50, viewer)
	if feed[0].ReplyCount != 1 || feed[0].LastReplyTS != 6000 {
		t.Errorf("с блоком: reply_count=%d last_reply_ts=%d, want 1/6000", feed[0].ReplyCount, feed[0].LastReplyTS)
	}
	if _, replies, _ := s.Replies(root, viewer); !slices.Equal(idsOf(replies), []int64{r2}) {
		t.Errorf("тред с блоком = %v, want только [%d]", idsOf(replies), r2)
	}

	// тред в один уровень: корень ответа — его parent, корень корня — он сам
	if got, ch, err := s.ThreadRoot(r1); err != nil || got != root || ch != "RU" {
		t.Errorf("ThreadRoot(ответ) = %d %q %v, want %d RU", got, ch, err, root)
	}
	if got, _, _ := s.ThreadRoot(root); got != root {
		t.Errorf("ThreadRoot(корень) = %d, want %d", got, root)
	}
	if _, _, err := s.ThreadRoot(999999); err != ErrMessageGone {
		t.Errorf("ThreadRoot(нет) err = %v, want ErrMessageGone", err)
	}
	if _, _, err := s.Replies(r1, 0); err != ErrMessageGone {
		t.Errorf("Replies по id ответа err = %v, want ErrMessageGone (приводит вызывающий)", err)
	}
}

// Удаление корня уносит тред целиком — автором, модератором и чисткой
// сообщений нарушителя. Удаление ответа корень не трогает.
func TestStoreThreadCascade(t *testing.T) {
	s := openTestStore(t)
	a := mkTgUser(t, s, "1", "", "Аня")
	b := mkTgUser(t, s, "2", "", "Боря")

	t.Run("автор удаляет корень", func(t *testing.T) {
		root := mkMessage(t, s, "RU", a, "корень")
		reply := mkReply(t, s, root, "RU", b, "чужой ответ", 1)
		if ok, err := s.DeleteOwnMessage(a, root); err != nil || !ok {
			t.Fatalf("delete: ok=%v err=%v", ok, err)
		}
		if exists(t, s, reply) {
			t.Fatal("ответ пережил удаление корня")
		}
	})
	t.Run("удаление ответа корень не трогает", func(t *testing.T) {
		root := mkMessage(t, s, "RU", a, "корень")
		reply := mkReply(t, s, root, "RU", b, "ответ", 1)
		if ok, _ := s.DeleteOwnMessage(b, reply); !ok {
			t.Fatal("ответ не удалился")
		}
		if !exists(t, s, root) {
			t.Fatal("корень удалился вместе с ответом")
		}
	})
	t.Run("модератор", func(t *testing.T) {
		root := mkMessage(t, s, "RU", a, "корень")
		reply := mkReply(t, s, root, "RU", b, "ответ", 1)
		s.DeleteMessage(root)
		if exists(t, s, reply) {
			t.Fatal("/del корня оставил ответ")
		}
	})
	t.Run("purge автора корня", func(t *testing.T) {
		root := mkMessage(t, s, "RU", a, "спам")
		reply := mkReply(t, s, root, "RU", b, "ответ на спам", 1)
		s.DeleteUserMessages(a)
		if exists(t, s, reply) {
			t.Fatal("/purge оставил ответы в тредах нарушителя")
		}
	})
}

// Тред живёт по последней активности: свежий ответ держит старый корень, а
// уходит тред целиком, когда устарело всё.
func TestStoreThreadTTL(t *testing.T) {
	s := openTestStore(t)
	a := mkTgUser(t, s, "1", "", "Аня")
	old := time.Now().Add(-messageTTL - time.Hour).UnixMilli()
	fresh := time.Now().UnixMilli()

	save := func(text string, ts int64) int64 {
		id, _, err := s.SaveMessage("RU", a, text, ts, "")
		if err != nil {
			t.Fatalf("save: %v", err)
		}
		return id
	}
	alive := save("старый корень, свежий ответ", old)
	aliveReply := mkReply(t, s, alive, "RU", a, "свежий", fresh)
	aliveOldReply := mkReply(t, s, alive, "RU", a, "старый ответ живого треда", old)
	dead := save("всё старое", old)
	deadReply := mkReply(t, s, dead, "RU", a, "старый", old)
	plainOld := save("старый без ответов", old)
	plainFresh := save("свежий", fresh)

	if _, err := s.DeleteMessagesOlderThan(messageTTL); err != nil {
		t.Fatalf("ttl: %v", err)
	}
	for id, want := range map[int64]bool{
		alive: true, aliveReply: true, aliveOldReply: true,
		dead: false, deadReply: false, plainOld: false, plainFresh: true,
	} {
		if got := exists(t, s, id); got != want {
			t.Errorf("сообщение %d: осталось=%v, want %v", id, got, want)
		}
	}
}

// Пуш об ответе — автору корня и тем, кто в треде отвечал; не самому
// отвечающему, не тем, кто его заблокировал, и не прочим подписчикам канала.
func TestStoreThreadPushTargets(t *testing.T) {
	s := openTestStore(t)
	author := mkTgUser(t, s, "1", "", "Автор")
	replier := mkTgUser(t, s, "2", "", "Отвечал")
	sender := mkTgUser(t, s, "3", "", "Пишет сейчас")
	blocker := mkTgUser(t, s, "4", "", "Заблокировал")
	bystander := mkTgUser(t, s, "5", "", "Просто в канале")
	for uid, tok := range map[int64]string{
		author: "t-author", replier: "t-replier", sender: "t-sender",
		blocker: "t-blocker", bystander: "t-bystander",
	} {
		if err := s.SaveDeviceToken(uid, tok, "ios"); err != nil {
			t.Fatalf("token: %v", err)
		}
	}
	root := mkMessage(t, s, "RU", author, "корень")
	mkReply(t, s, root, "RU", replier, "раз", 1)
	mkReply(t, s, root, "RU", blocker, "два", 2)
	mkReply(t, s, root, "RU", sender, "три", 3)
	if err := s.BlockUser(blocker, sender); err != nil {
		t.Fatalf("block: %v", err)
	}

	got, err := s.ThreadPushTargets(root, sender)
	if err != nil {
		t.Fatalf("targets: %v", err)
	}
	slices.Sort(got)
	if want := []string{"t-author", "t-replier"}; !slices.Equal(got, want) {
		t.Fatalf("получатели = %v, want %v", got, want)
	}
}

// Отметка ответа ведёт в тред: уведомлению нужен корень.
func TestVoteNoticeRootID(t *testing.T) {
	s := openTestStore(t)
	a := mkTgUser(t, s, "1", "", "Аня")
	b := mkTgUser(t, s, "2", "", "Боря")
	root := mkMessage(t, s, "RU", a, "корень")
	reply := mkReply(t, s, root, "RU", a, "ответ", 1)

	out, err := s.Vote(b, reply, 1)
	if err != nil || out.Notice == nil || out.Notice.RootID != root {
		t.Fatalf("голос за ответ: notice=%+v err=%v, want RootID %d", out.Notice, err, root)
	}
	out, _ = s.Vote(b, root, 1)
	if out.Notice == nil || out.Notice.RootID != 0 {
		t.Fatalf("голос за корень: notice=%+v, want RootID 0", out.Notice)
	}
}

// threadServer — POST /messages с лимитером и GET треда, плюс подписчик канала
// RU в хабе, чтобы видеть рассылку.
func threadServer(t *testing.T) (*httptest.Server, *Store, *Client) {
	t.Helper()
	_, store := newTestServer(t)
	hub := NewHub()
	go hub.Run()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /messages", handlePublish(store,
		&publisher{store: store, hub: hub, limiter: NewRateLimiter()}))
	mux.HandleFunc("GET /messages", handleHistory(store))
	mux.HandleFunc("GET /messages/{id}/replies", handleReplies(store))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	sub := &Client{send: make(chan Envelope, 32)}
	hub.subscribe <- subscription{client: sub, channels: []string{"RU", PlanetChannel.ID}}
	return srv, store, sub
}

func nextFrame(t *testing.T, c *Client) Envelope {
	t.Helper()
	select {
	case env := <-c.send:
		return env
	case <-time.After(2 * time.Second):
		t.Fatal("кадр не пришёл")
		return Envelope{}
	}
}

func getJSON(t *testing.T, url string, v any) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	return resp.StatusCode
}

// Ответ через POST /messages: канал берётся из корня, ответ на ответ встаёт в
// тред корня, рассылка — кадром reply (выпущенные сборки его пропускают), в
// ленте канала ответа нет, в треде есть.
func TestPublishReply(t *testing.T) {
	srv, store, sub := threadServer(t)
	userID, token := publishSession(t, store)
	ageAccount(t, store, userID)

	resp, body := postMessage(t, srv.URL, token, PublishRequest{Channel: "RU", Text: "корень"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("корень: %d (%v)", resp.StatusCode, body)
	}
	if env := nextFrame(t, sub); env.Type != TypeMessage {
		t.Fatalf("корень пришёл кадром %q", env.Type)
	}
	root := int64(body["message"].(map[string]any)["id"].(float64))

	// канал из запроса игнорируется: ответ не может уехать в чужой канал
	resp, body = postMessage(t, srv.URL, token, PublishRequest{
		Channel: "DE", Text: "ответ", ParentID: root,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ответ: %d (%v)", resp.StatusCode, body)
	}
	msg := body["message"].(map[string]any)
	if msg["channel"] != "RU" || int64(msg["parent_id"].(float64)) != root {
		t.Fatalf("ответ = %v, want канал RU и parent_id %d", msg, root)
	}
	reply := int64(msg["id"].(float64))
	env := nextFrame(t, sub)
	if env.Type != TypeReply {
		t.Fatalf("ответ пришёл кадром %q, want %q", env.Type, TypeReply)
	}
	var frame MessageData
	mustUnmarshal(t, env.Data, &frame)
	if frame.ParentID != root || frame.ID != reply {
		t.Fatalf("кадр reply = %+v", frame)
	}

	// ответ на ответ — в тот же тред
	resp, body = postMessage(t, srv.URL, token, PublishRequest{Text: "на ответ", ParentID: reply})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ответ на ответ: %d (%v)", resp.StatusCode, body)
	}
	if got := int64(body["message"].(map[string]any)["parent_id"].(float64)); got != root {
		t.Fatalf("ответ на ответ: parent_id = %d, want корень %d", got, root)
	}

	var feed HistoryData
	getJSON(t, srv.URL+"/messages?channel=RU", &feed)
	if len(feed.Messages) != 1 || feed.Messages[0].ID != root || feed.Messages[0].ReplyCount != 2 {
		t.Fatalf("лента = %+v, want один корень с двумя ответами", feed.Messages)
	}

	var thread RepliesData
	// и по id ответа тред находится по корню
	if code := getJSON(t, srv.URL+"/messages/"+itoa(reply)+"/replies", &thread); code != http.StatusOK {
		t.Fatalf("тред: status %d", code)
	}
	if thread.Root.ID != root || len(thread.Replies) != 2 {
		t.Fatalf("тред = %+v", thread)
	}

	// корня нет — ответить некуда
	store.DeleteMessage(root)
	resp, body = postMessage(t, srv.URL, token, PublishRequest{Text: "поздно", ParentID: root})
	if resp.StatusCode != http.StatusNotFound || body["code"] != "not_found" {
		t.Fatalf("ответ в удалённый тред: %d/%v, want 404/not_found", resp.StatusCode, body["code"])
	}
	if code := getJSON(t, srv.URL+"/messages/"+itoa(root)+"/replies", &thread); code != http.StatusNotFound {
		t.Fatalf("тред удалённого корня: status %d, want 404", code)
	}
	resp, _ = postMessage(t, srv.URL, token, PublishRequest{Text: "x", ParentID: -1})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("parent_id < 0: status %d, want 400", resp.StatusCode)
	}
}

// Ответы — своим бакетом по городскому тиру в любом канале: исчерпанная Земля
// не закрывает её же треды, а исчерпанный тред не закрывает каналы.
func TestPublishReplyLimit(t *testing.T) {
	srv, store, _ := threadServer(t)
	userID, token := publishSession(t, store)
	ageAccount(t, store, userID)

	resp, body := postMessage(t, srv.URL, token, PublishRequest{Channel: PlanetChannel.ID, Text: "всем"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("в Землю: %d (%v)", resp.StatusCode, body)
	}
	root := int64(body["message"].(map[string]any)["id"].(float64))
	if resp, _ := postMessage(t, srv.URL, token, PublishRequest{Channel: PlanetChannel.ID, Text: "ещё"}); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("второе в Землю: %d, want 429", resp.StatusCode)
	}

	for i := 0; i < cityLimit.capacity; i++ {
		if resp, body := postMessage(t, srv.URL, token, PublishRequest{Text: "в треде", ParentID: root}); resp.StatusCode != http.StatusOK {
			t.Fatalf("ответ %d в треде Земли: %d (%v)", i+1, resp.StatusCode, body)
		}
	}
	resp, body = postMessage(t, srv.URL, token, PublishRequest{Text: "сверх", ParentID: root})
	if resp.StatusCode != http.StatusTooManyRequests || body["code"] != "too_fast" {
		t.Fatalf("после всплеска в треде: %d/%v, want 429/too_fast", resp.StatusCode, body["code"])
	}
	if msg, _ := body["message"].(string); !strings.Contains(msg, "тред") {
		t.Errorf("текст отказа = %q, ожидали объяснение про треды", msg)
	}
	if resp, body := postMessage(t, srv.URL, token, PublishRequest{Channel: "RU", Text: "в страну"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("исчерпанный тред закрыл страну: %d (%v)", resp.StatusCode, body)
	}
}

// Правило «в Землю через сутки» действует и на её треды: иначе свежий аккаунт
// писал бы на весь мир ответами.
func TestPublishReplyPlanetNewAccount(t *testing.T) {
	srv, store, _ := threadServer(t)
	old := mkTgUser(t, store, "100", "", "Старожил")
	ageAccount(t, store, old)
	root := mkMessage(t, store, PlanetChannel.ID, old, "всем")

	_, token := publishSession(t, store) // свежий
	resp, body := postMessage(t, srv.URL, token, PublishRequest{Text: "ответ", ParentID: root})
	if resp.StatusCode != http.StatusTooManyRequests || !strings.Contains(body["message"].(string), "через сутки") {
		t.Fatalf("новичок в треде Земли: %d/%v, want 429 про сутки", resp.StatusCode, body["message"])
	}
	// в треде локального канала можно сразу
	local := mkMessage(t, store, "RU", old, "своим")
	if resp, body := postMessage(t, srv.URL, token, PublishRequest{Text: "ответ", ParentID: local}); resp.StatusCode != http.StatusOK {
		t.Fatalf("новичок в треде страны: %d (%v)", resp.StatusCode, body)
	}
}

// Повтор отправки ответа с тем же client_msg_id отдаёт сохранённый ответ —
// вместе с parent_id, иначе клиент положил бы его в ленту канала.
func TestPublishReplyIdempotent(t *testing.T) {
	srv, store, _ := threadServer(t)
	userID, token := publishSession(t, store)
	ageAccount(t, store, userID)
	root := mkMessage(t, store, "RU", userID, "корень")

	req := PublishRequest{Text: "ответ", ParentID: root, ClientMsgID: "r-1"}
	postMessage(t, srv.URL, token, req)
	resp, body := postMessage(t, srv.URL, token, req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("повтор: %d (%v)", resp.StatusCode, body)
	}
	if got := int64(body["message"].(map[string]any)["parent_id"].(float64)); got != root {
		t.Fatalf("повтор: parent_id = %d, want %d", got, root)
	}
	_, replies, _ := store.Replies(root, 0)
	if len(replies) != 1 {
		t.Fatalf("повтор создал дубль: %d ответов", len(replies))
	}
}
