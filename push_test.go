package main

import (
	"encoding/json"
	"testing"
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
			"relation/2555133", "Тверской"))
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
		decode(t, pushPayload("dev-token", "Ваня", "привет", "RU-MOW", ""))
		if got.Message.Data["channel"] != "RU-MOW" {
			t.Fatalf("data.channel = %q", got.Message.Data["channel"])
		}
		if _, ok := got.Message.Data["channel_name"]; ok {
			t.Fatalf("channel_name не должен присутствовать: %v", got.Message.Data)
		}
	})
}
