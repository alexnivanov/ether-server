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

// Pusher шлёт пуши о новых сообщениях через FCM HTTP v1 — **адресно, по токенам
// устройств**. Раньше модель была «топик = канал», но в топик нельзя не
// отправить конкретному подписчику, и автор получал уведомление о своём же
// сообщении; заодно fan-out по топику давал задержку в минуты на Android.
// Теперь сервер знает каналы каждого пользователя (user_channels, обновляются на
// locate) и токены его устройств (device_tokens), поэтому получателей считает сам
// и исключает автора (см. Store.PushTargets).
//
// Пуши опциональны: без service-account JSON в конфиге Pusher не создаётся
// (nil), и publish работает как раньше, просто без уведомлений.
type Pusher struct {
	projectID string
	ts        oauth2.TokenSource
	http      *http.Client
	store     *Store // нужен, чтобы вычислить получателей и убрать мёртвые токены
}

// NewPusher читает service-account JSON. Пустой credsFile → (nil, nil): пуши
// выключены. Ошибку чтения/парсинга возвращаем — вызывающий решает, что делать
// (в main это лог + работа без пушей, не фатал).
func NewPusher(projectID, credsFile string, store *Store) (*Pusher, error) {
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
		store:     store,
	}, nil
}

// Notify рассылает уведомление о новом сообщении всем устройствам подписчиков
// канала, кроме автора. Задумана для вызова в горутине: блокируется на HTTP к
// FCM (по запросу на токен — в HTTP v1 нет мультикаста), а доставка сообщения по
// WS от пуша не зависит, поэтому ошибки только логируем.
func (p *Pusher) Notify(channelID string, senderTgID int64, sender, text string) {
	tokens, err := p.store.PushTargets(channelID, senderTgID)
	if err != nil {
		slog.Error("push targets", "err", err, "channel", channelID)
		return
	}
	if len(tokens) == 0 {
		return // некому: в канале нет других устройств
	}
	tok, err := p.ts.Token()
	if err != nil {
		slog.Error("fcm token", "err", err)
		return
	}

	var sent int
	var stale []string
	for _, device := range tokens {
		switch p.sendTo(tok.AccessToken, device, sender, text) {
		case sendOK:
			sent++
		case sendStale:
			// приложение удалено/токен перевыпущен — чтобы не долбить FCM зря
			stale = append(stale, device)
		}
	}
	if len(stale) > 0 {
		if err := p.store.DeleteDeviceTokens(stale); err != nil {
			slog.Error("delete stale tokens", "err", err)
		}
	}
	slog.Info("fcm send", "channel", channelID, "sent", sent,
		"targets", len(tokens), "stale", len(stale))
}

type sendResult int

const (
	sendOK sendResult = iota
	sendFailed
	sendStale // токен больше не существует — удалить из БД
)

// sendTo отправляет одно уведомление на один токен устройства.
func (p *Pusher) sendTo(accessToken, device, sender, text string) sendResult {
	payload, _ := json.Marshal(map[string]any{
		"message": map[string]any{
			"token": device,
			"notification": map[string]any{
				"title": sender,
				"body":  text,
			},
		},
	})
	url := fmt.Sprintf("https://fcm.googleapis.com/v1/projects/%s/messages:send", p.projectID)
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.http.Do(req)
	if err != nil {
		slog.Error("fcm send", "err", err)
		return sendFailed
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return sendOK
	}
	b, _ := io.ReadAll(resp.Body)
	body := string(b)
	// 404 UNREGISTERED / 400 с невалидным токеном — устройство больше не наше
	if resp.StatusCode == http.StatusNotFound || strings.Contains(body, "UNREGISTERED") ||
		strings.Contains(body, "INVALID_ARGUMENT") {
		slog.Info("fcm token stale", "status", resp.Status)
		return sendStale
	}
	slog.Warn("fcm send rejected", "status", resp.Status, "body", body)
	return sendFailed
}
