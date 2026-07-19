package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// Pusher шлёт пуши о новых сообщениях через FCM HTTP v1. Модель — **топик =
// канал**: клиент подписывается на FCM-топики только своих Района и Квартала,
// поэтому сервер просто шлёт в топик каждого опубликованного сообщения — на
// топики города/области/страны подписчиков нет, туда доставка no-op. Так
// уровневая политика (пуши только Район+Квартал) держится на стороне клиента,
// серверу знать уровень канала не нужно.
//
// Пуши опциональны: без service-account JSON в конфиге Pusher не создаётся
// (nil), и publish работает как раньше, просто без уведомлений.
type Pusher struct {
	projectID string
	ts        oauth2.TokenSource
	http      *http.Client
}

// NewPusher читает service-account JSON. Пустой credsFile → (nil, nil): пуши
// выключены. Ошибку чтения/парсинга возвращаем — вызывающий решает, что делать
// (в main это лог + работа без пушей, не фатал).
func NewPusher(projectID, credsFile string) (*Pusher, error) {
	if credsFile == "" {
		return nil, nil
	}
	data, err := os.ReadFile(credsFile)
	if err != nil {
		return nil, err
	}
	creds, err := google.CredentialsFromJSON(context.Background(), data,
		"https://www.googleapis.com/auth/firebase.messaging")
	if err != nil {
		return nil, err
	}
	return &Pusher{
		projectID: projectID,
		ts:        creds.TokenSource, // сам обновляет access-token по мере протухания
		http:      &http.Client{Timeout: 10 * time.Second},
	}, nil
}

// fcmTopic приводит ID канала к допустимому имени топика FCM (разрешено только
// [a-zA-Z0-9-_.~%]). В наших ID из спецсимволов встречается только "/"
// (osm_type/osm_id, напр. "relation/2555133" → "c_relation_2555133"); ISO-коды
// (RU, RU-MOW) проходят как есть. Клиент обязан санитайзить точно так же.
func fcmTopic(channelID string) string {
	return "c_" + strings.ReplaceAll(channelID, "/", "_")
}

// Notify шлёт пуш в топик канала (title — отправитель, body — текст). Задуман
// для вызова в горутине: блокируется на HTTP к FCM, а доставка сообщения по WS
// от пуша не зависит, поэтому ошибки только логируем.
func (p *Pusher) Notify(channelID, sender, text string) {
	tok, err := p.ts.Token()
	if err != nil {
		slog.Error("fcm token", "err", err)
		return
	}
	topic := fcmTopic(channelID)
	payload, _ := json.Marshal(map[string]any{
		"message": map[string]any{
			"topic": topic,
			"notification": map[string]any{
				"title": sender,
				"body":  text,
			},
		},
	})
	url := fmt.Sprintf("https://fcm.googleapis.com/v1/projects/%s/messages:send", p.projectID)
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.http.Do(req)
	if err != nil {
		slog.Error("fcm send", "topic", topic, "err", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		slog.Warn("fcm send rejected", "topic", topic, "status", resp.Status, "body", string(b))
		return
	}
	// лог и на успехе — чтобы было видно каждую отправку (топик + ok)
	slog.Info("fcm send", "topic", topic, "status", "ok")
}
