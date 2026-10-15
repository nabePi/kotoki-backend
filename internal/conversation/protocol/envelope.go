// Package protocol mendefinisikan protokol WebSocket v1 (spec §7.4): envelope,
// payload, registry tipe, dan codec JSON. Paket ini murni Go (tanpa cgo) dan
// tidak mengimpor conversation/audio.
package protocol

import (
	"time"

	"github.com/google/uuid"
)

// Version adalah versi protokol (§7.4.2).
const Version = 1

// tsLayout: RFC 3339 UTC dengan presisi milidetik tetap (§7.4.2).
const tsLayout = "2006-01-02T15:04:05.000Z"

// ServerEvent adalah envelope server -> client.
type ServerEvent struct {
	Type       string
	Version    int
	EventID    uuid.UUID
	ServerSeq  int64
	SessionID  uuid.UUID
	TurnID     *uuid.UUID
	TurnSeq    *int
	Generation *uint32
	TS         time.Time
	Replay     bool
	Payload    any
}

// ClientEvent adalah envelope client -> server.
type ClientEvent struct {
	Type      string
	Version   int
	EventID   uuid.UUID
	SessionID uuid.UUID
	TS        time.Time
	Payload   any
}

// Timestamp adalah waktu dalam payload yang selalu ditulis RFC 3339 UTC dengan
// tepat tiga digit milidetik.
type Timestamp struct{ time.Time }

// MarshalJSON menulis format tetap milidetik.
func (t Timestamp) MarshalJSON() ([]byte, error) {
	return []byte(`"` + t.UTC().Format(tsLayout) + `"`), nil
}

// UnmarshalJSON menerima RFC 3339 apa pun.
func (t *Timestamp) UnmarshalJSON(b []byte) error {
	return t.Time.UnmarshalJSON(b)
}

type serverWire struct {
	Type       string     `json:"type"`
	Version    int        `json:"version"`
	EventID    uuid.UUID  `json:"event_id"`
	ServerSeq  int64      `json:"server_seq"`
	SessionID  uuid.UUID  `json:"session_id"`
	TurnID     *uuid.UUID `json:"turn_id,omitempty"`
	TurnSeq    *int       `json:"turn_seq,omitempty"`
	Generation *uint32    `json:"generation,omitempty"`
	TS         string     `json:"ts"`
	Replay     bool       `json:"replay,omitempty"`
	Payload    any        `json:"payload"`
}

type clientWire struct {
	Type      string    `json:"type"`
	Version   int       `json:"version"`
	EventID   uuid.UUID `json:"event_id"`
	SessionID uuid.UUID `json:"session_id"`
	TS        string    `json:"ts,omitempty"`
	Payload   any       `json:"payload"`
}
