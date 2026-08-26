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
func (p *Pusher) Notify(channelID string, senderID int64, sender, text string) {
	tokens, err := p.store.PushTargets(channelID, senderID)
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
	// Имя канала — из справочника (заполняется на locate, см. Store.SaveChannels):
	// в сообщении лежит только ID, а человеку нужно понять, откуда оно пришло.
	// Ошибка и пустое имя равносильны: уведомление уйдёт без названия канала.
	name, err := p.store.ChannelName(channelID)
	if err != nil {
		slog.Error("channel name", "err", err, "channel", channelID)
	}
	title, body := pushText(name, sender, text)

	var sent int
	var stale []string
	for _, device := range tokens {
		switch p.sendTo(tok.AccessToken, device, title, body) {
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

// pushText — что человек увидит в уведомлении. Заголовок — КАНАЛ, а не автор:
// так же устроены групповые чаты, и ОС группирует уведомления по заголовку —
// значит по каналу, а не по случайному соседу.
//
// Имя канала пустое (справочник ещё не знает этот ID, база жила до его
// появления) — остаётся прежний вид, автор в заголовке: уведомление без
// названия лучше, чем уведомление с пустой первой строкой.
func pushText(channel, sender, text string) (title, body string) {
	if channel == "" {
		return sender, text
	}
	return channel, sender + ": " + text
}

type sendResult int

const (
	sendOK sendResult = iota
	sendFailed
	sendStale // токен больше не существует — удалить из БД
)

// sendTo отправляет одно уведомление на один токен устройства. title/body уже
// готовы (см. pushText) — здесь только транспорт.
func (p *Pusher) sendTo(accessToken, device, title, body string) sendResult {
	payload, _ := json.Marshal(map[string]any{
		"message": map[string]any{
			"token": device,
			"notification": map[string]any{
				"title": title,
				"body":  body,
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
	reason := string(b)
	// 404 UNREGISTERED / 400 с невалидным токеном — устройство больше не наше
	if resp.StatusCode == http.StatusNotFound || strings.Contains(reason, "UNREGISTERED") ||
		strings.Contains(reason, "INVALID_ARGUMENT") {
		slog.Info("fcm token stale", "status", resp.Status)
		return sendStale
	}
	slog.Warn("fcm send rejected", "status", resp.Status, "body", reason)
	return sendFailed
}
