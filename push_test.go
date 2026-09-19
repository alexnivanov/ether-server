package main

import (
	"encoding/json"
	"testing"
	"unicode/utf8"
)

// Текст уведомления: канал в заголовке, автор — перед сообщением. Заголовок
// важен не только читаемостью: по нему ОС группирует уведомления, поэтому в
// шторке они собираются по каналу.
func TestPushText(t *testing.T) {
	tests := []struct {
		name      string
		channel   string
		sender    string
		text      string
		wantTitle string
		wantBody  string
	}{
		{
			name:      "с именем канала",
			channel:   "Тверской",
			sender:    "Ваня",
			text:      "пойдём гулять",
			wantTitle: "Тверской",
			wantBody:  "Ваня: пойдём гулять",
		},
		{
			// справочник ещё не знает канал (база жила до его появления, в этом
			// месте никто не делал locate) — прежний вид, автор в заголовке
			name:      "без имени канала",
			channel:   "",
			sender:    "Ваня",
			text:      "пойдём гулять",
			wantTitle: "Ваня",
			wantBody:  "пойдём гулять",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			title, body := pushText(tt.channel, tt.sender, tt.text)
			if title != tt.wantTitle || body != tt.wantBody {
				t.Fatalf("%q / %q, want %q / %q", title, body, tt.wantTitle, tt.wantBody)
			}
		})
	}
}

// Тело запроса к FCM — контракт с клиентом (ether-meta/PROTOCOL.md): по
// `data.channel` приложение открывает нужную комнату, а по `data.channel_name`
// называет зону, если человека в ней уже нет. Проверяем на JSON: собранное тело
// и есть то, что увидит клиент.
func TestPushPayload(t *testing.T) {
	var got struct {
		Message struct {
			Token        string            `json:"token"`
			Notification map[string]string `json:"notification"`
			Data         map[string]string `json:"data"`
		} `json:"message"`
	}
	decode := func(t *testing.T, b []byte) {
		t.Helper()
		got = struct {
			Message struct {
				Token        string            `json:"token"`
				Notification map[string]string `json:"notification"`
				Data         map[string]string `json:"data"`
			} `json:"message"`
		}{}
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("payload не разбирается: %v", err)
		}
	}

	t.Run("канал и его имя едут в data", func(t *testing.T) {
		decode(t, pushPayload("dev-token", "Тверской", "Ваня: привет",
			channelData("relation/2555133", "Тверской")))
		if got.Message.Token != "dev-token" {
			t.Fatalf("token = %q", got.Message.Token)
		}
		if got.Message.Notification["title"] != "Тверской" ||
			got.Message.Notification["body"] != "Ваня: привет" {
			t.Fatalf("notification = %v", got.Message.Notification)
		}
		if got.Message.Data["channel"] != "relation/2555133" {
			t.Fatalf("data.channel = %q", got.Message.Data["channel"])
		}
		if got.Message.Data["channel_name"] != "Тверской" {
			t.Fatalf("data.channel_name = %q", got.Message.Data["channel_name"])
		}
	})

	// Справочник ещё не знает канал — имени нет. Пустую строку не шлём:
	// клиенту «поля нет» и «поле пустое» означают одно и то же.
	t.Run("без имени канала поля нет вовсе", func(t *testing.T) {
		decode(t, pushPayload("dev-token", "Ваня", "привет", channelData("RU-MOW", "")))
		if got.Message.Data["channel"] != "RU-MOW" {
			t.Fatalf("data.channel = %q", got.Message.Data["channel"])
		}
		if _, ok := got.Message.Data["channel_name"]; ok {
			t.Fatalf("channel_name не должен присутствовать: %v", got.Message.Data)
		}
	})
}

// Текст уведомления об отметке. Знак голоса в нём не называется намеренно (см.
// NotifyVote), а фрагмент своего сообщения нужен, чтобы автор понял, о каком
// из них речь.
func TestVoteText(t *testing.T) {
	t.Run("канал в заголовке, сообщение в теле", func(t *testing.T) {
		title, body := voteText("Тверской", "пойдём гулять")
		if title != "Тверской" {
			t.Fatalf("title = %q", title)
		}
		if body != "Твоё сообщение отметили: пойдём гулять" {
			t.Fatalf("body = %q", body)
		}
	})

	// Пустая первая строка в шторке выглядит сбоем, поэтому вместо неё имя
	// приложения.
	t.Run("без имени канала — имя приложения", func(t *testing.T) {
		title, _ := voteText("", "привет")
		if title != "Эфир" {
			t.Fatalf("title = %q", title)
		}
	})

	// Обрезка по рунам, а не по байтам: кириллица иначе разваливается пополам.
	t.Run("длинное сообщение обрезано по рунам", func(t *testing.T) {
		long := ""
		for range maxVoteText + 20 {
			long += "я"
		}
		_, body := voteText("Тверской", long)
		text := body[len("Твоё сообщение отметили: "):]
		if r := []rune(text); len(r) != maxVoteText+1 || string(r[maxVoteText]) != "…" {
			t.Fatalf("обрезано до %d рун: %q", len([]rune(text)), text)
		}
		if !utf8.ValidString(body) {
			t.Fatalf("битая строка: %q", body)
		}
	})
}

// Уведомление об отметке — тот же контракт, что и уведомление о сообщении, плюс
// два поля: по type клиент отличает одно от другого, по message_id ведёт тап к
// самому сообщению, а не просто в комнату (ether-meta/PROTOCOL.md).
func TestVotePushPayload(t *testing.T) {
	data := channelData("relation/2555133", "Тверской")
	data["type"] = "vote"
	data["message_id"] = "42"

	var got struct {
		Message struct {
			Notification map[string]string `json:"notification"`
			Data         map[string]string `json:"data"`
		} `json:"message"`
	}
	if err := json.Unmarshal(
		pushPayload("dev-token", "Тверской", "Твоё сообщение отметили: привет", data),
		&got); err != nil {
		t.Fatalf("payload не разбирается: %v", err)
	}
	if got.Message.Data["type"] != "vote" || got.Message.Data["message_id"] != "42" {
		t.Fatalf("data = %v", got.Message.Data)
	}
	// Канал остаётся на месте: сборки, которые про type ещё не знают, читают
	// только его и открывают комнату, как раньше.
	if got.Message.Data["channel"] != "relation/2555133" {
		t.Fatalf("data.channel = %q", got.Message.Data["channel"])
	}
	if got.Message.Notification["title"] != "Тверской" {
		t.Fatalf("notification = %v", got.Message.Notification)
	}
}

// Дедуп: голос можно снять и поставить заново, и каждый раз это новая строка в
// votes. Автора будим один раз на пару «проголосовавший + сообщение», иначе
// качелями ему устраивают поток уведомлений (см. Pusher.notified).
func TestVoteNoticeIsOncePerPair(t *testing.T) {
	p := &Pusher{notified: map[string]struct{}{}}
	if !p.firstVoteNotice(7, 42) {
		t.Fatal("первая отметка должна уведомлять")
	}
	if p.firstVoteNotice(7, 42) {
		t.Fatal("повтор той же пары уведомлять не должен")
	}
	// Другой человек под тем же сообщением — новая реакция, она уведомляет.
	if !p.firstVoteNotice(8, 42) {
		t.Fatal("другой голосующий должен уведомлять")
	}
	// Тот же человек под другим сообщением — тоже.
	if !p.firstVoteNotice(7, 43) {
		t.Fatal("другое сообщение должно уведомлять")
	}
}

// Набор пар не растёт бесконечно: на потолке он сбрасывается целиком. Цена
// сброса — одно лишнее уведомление, цена его отсутствия — память, которую
// никто не освобождает.
func TestVoteNoticesAreCapped(t *testing.T) {
	p := &Pusher{notified: map[string]struct{}{}}
	for i := range maxVoteNotices {
		p.firstVoteNotice(int64(i), 1)
	}
	if len(p.notified) != maxVoteNotices {
		t.Fatalf("набор = %d, want %d", len(p.notified), maxVoteNotices)
	}
	p.firstVoteNotice(maxVoteNotices, 1)
	if len(p.notified) != 1 {
		t.Fatalf("после сброса набор = %d, want 1", len(p.notified))
	}
}
