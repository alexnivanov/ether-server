package main

import (
	"strings"
	"testing"
)

func TestParseConfig(t *testing.T) {
	cfg, err := parseConfig([]byte(`
# комментарии — то, ради чего конфиг в YAML
telegram_client_id: "123"
google_client_ids:
  - "a.apps.googleusercontent.com"
  - "b.apps.googleusercontent.com"
db: "/var/lib/ether/ether.prod.db"
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.TelegramClientID != "123" || len(cfg.GoogleClientIDs) != 2 {
		t.Fatalf("cfg = %+v", cfg)
	}
	if cfg.Addr != ":8080" {
		t.Errorf("addr = %q, want умолчание :8080", cfg.Addr)
	}
}

// Опечатка в ключе должна валить старт, а не молча оставлять поле пустым:
// иначе выключенные уведомления/пуши обнаруживаются только по их отсутствию.
func TestParseConfigRejectsUnknownField(t *testing.T) {
	_, err := parseConfig([]byte("telegram_client_id: \"1\"\ntelegram_notify_chatid: \"-100\"\n"))
	if err == nil {
		t.Fatal("опечатка в ключе принята")
	}
	if !strings.Contains(err.Error(), "telegram_notify_chatid") {
		t.Errorf("ошибка не называет поле: %v", err)
	}
}

func TestParseConfigRequiresProvider(t *testing.T) {
	if _, err := parseConfig([]byte(`addr: ":9090"`)); err == nil {
		t.Fatal("конфиг без провайдеров входа принят")
	}
}

// YAML 1.2 — надмножество JSON, поэтому старый config.prod.json читается как
// YAML без правок: миграция на сервере — это переименование файла.
func TestParseConfigAcceptsJSON(t *testing.T) {
	cfg, err := parseConfig([]byte(`{"addr":":9090","telegram_client_id":"456","google_client_ids":["x"]}`))
	if err != nil {
		t.Fatalf("json как yaml: %v", err)
	}
	if cfg.Addr != ":9090" || cfg.TelegramClientID != "456" || len(cfg.GoogleClientIDs) != 1 {
		t.Fatalf("cfg = %+v", cfg)
	}
}
