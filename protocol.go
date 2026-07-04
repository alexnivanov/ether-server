package main

import "encoding/json"

// Wire-протокол — см. ether-meta/PROTOCOL.md. Каждый кадр WebSocket — это
// Envelope: тег типа + сырой payload, который доразбирается по типу.
const (
	// client → server
	TypeHello   = "hello"   // {nick}
	TypeLocate  = "locate"  // {lat, lng}
	TypePublish = "publish" // {channel, text}

	// server → client
	TypeLocated = "located" // {channels: [...]}
	TypeMessage = "message" // {channel, sender, text, ts}
	TypeError   = "error"   // {code, message}
)

// Envelope — внешняя оболочка любого сообщения.
type Envelope struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data,omitempty"`
}

// client → server
type HelloData struct {
	Nick string `json:"nick"`
}
type LocateData struct {
	Lat float64 `json:"lat"`
	Lng float64 `json:"lng"`
}
type PublishData struct {
	Channel string `json:"channel"`
	Text    string `json:"text"`
}

// server → client
type LocatedData struct {
	Channels []Channel `json:"channels"`
}
type MessageData struct {
	Channel string `json:"channel"`
	Sender  string `json:"sender"`
	Text    string `json:"text"`
	TS      int64  `json:"ts"`
}
type ErrorData struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// mustJSON сериализует payload в RawMessage для вложения в Envelope.
func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func envelope(typ string, data any) Envelope {
	return Envelope{Type: typ, Data: mustJSON(data)}
}
