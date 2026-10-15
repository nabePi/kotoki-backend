package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/google/uuid"
)

// Error sentinel decoder.
var (
	ErrUnknownType        = errors.New("protocol: unknown event type")
	ErrUnsupportedVersion = errors.New("protocol: unsupported version")
	ErrInvalidPayload     = errors.New("protocol: invalid payload")
)

// MarshalServer meng-encode envelope server (field kondisional dihilangkan bila nil).
func MarshalServer(ev ServerEvent) ([]byte, error) {
	v := ev.Version
	if v == 0 {
		v = Version
	}
	return json.Marshal(serverWire{
		Type: ev.Type, Version: v, EventID: ev.EventID, ServerSeq: ev.ServerSeq, SessionID: ev.SessionID,
		TurnID: ev.TurnID, TurnSeq: ev.TurnSeq, Generation: ev.Generation,
		TS: ev.TS.UTC().Format(tsLayout), Replay: ev.Replay, Payload: ev.Payload,
	})
}

// MarshalClient meng-encode envelope client.
func MarshalClient(ev ClientEvent) ([]byte, error) {
	v := ev.Version
	if v == 0 {
		v = Version
	}
	w := clientWire{Type: ev.Type, Version: v, EventID: ev.EventID, SessionID: ev.SessionID, Payload: ev.Payload}
	if !ev.TS.IsZero() {
		w.TS = ev.TS.UTC().Format(tsLayout)
	}
	return json.Marshal(w)
}

type rawEnvelope struct {
	Type       string          `json:"type"`
	Version    int             `json:"version"`
	EventID    uuid.UUID       `json:"event_id"`
	ServerSeq  int64           `json:"server_seq"`
	SessionID  uuid.UUID       `json:"session_id"`
	TurnID     *uuid.UUID      `json:"turn_id"`
	TurnSeq    *int            `json:"turn_seq"`
	Generation *uint32         `json:"generation"`
	TS         *time.Time      `json:"ts"`
	Replay     bool            `json:"replay"`
	Payload    json.RawMessage `json:"payload"`
}

func decodeEnvelope(data []byte, registry map[string]reflect.Type) (rawEnvelope, any, error) {
	var env rawEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return env, nil, fmt.Errorf("%w: envelope: %w", ErrInvalidPayload, err)
	}
	if env.Version != Version {
		return env, nil, fmt.Errorf("%w: %d", ErrUnsupportedVersion, env.Version)
	}
	typ, ok := registry[env.Type]
	if !ok {
		return env, nil, fmt.Errorf("%w: %q", ErrUnknownType, env.Type)
	}
	if len(env.Payload) == 0 || env.Payload[0] != '{' {
		return env, nil, fmt.Errorf("%w: payload bukan object", ErrInvalidPayload)
	}
	p := reflect.New(typ)
	// Field tak dikenal sengaja diabaikan (§7.4.2, forward compatibility).
	if err := json.Unmarshal(env.Payload, p.Interface()); err != nil {
		return env, nil, fmt.Errorf("%w: %s: %w", ErrInvalidPayload, env.Type, err)
	}
	return env, p.Elem().Interface(), nil
}

// DecodeClientEvent men-decode envelope client; payload bertipe nilai (mis. Ping).
func DecodeClientEvent(data []byte) (ClientEvent, error) {
	env, payload, err := decodeEnvelope(data, clientTypes)
	if err != nil {
		return ClientEvent{}, err
	}
	ev := ClientEvent{Type: env.Type, Version: env.Version, EventID: env.EventID, SessionID: env.SessionID, Payload: payload}
	if env.TS != nil {
		ev.TS = *env.TS
	}
	return ev, nil
}

// DecodeServerEvent men-decode envelope server (untuk client Go/tes).
func DecodeServerEvent(data []byte) (ServerEvent, error) {
	env, payload, err := decodeEnvelope(data, serverTypes)
	if err != nil {
		return ServerEvent{}, err
	}
	ev := ServerEvent{
		Type: env.Type, Version: env.Version, EventID: env.EventID, ServerSeq: env.ServerSeq, SessionID: env.SessionID,
		TurnID: env.TurnID, TurnSeq: env.TurnSeq, Generation: env.Generation, Replay: env.Replay, Payload: payload,
	}
	if env.TS != nil {
		ev.TS = *env.TS
	}
	return ev, nil
}
