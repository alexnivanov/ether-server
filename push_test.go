package main

import "testing"

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
