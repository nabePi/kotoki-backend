// Package probe holds the Go-native probes of the chaos harness: clients that do something the
// command line tools cannot, and fail when what they observe is wrong.
package probe

import (
	"errors"
	"fmt"
	"sort"
)

// Event is what the probe keeps of a server event.
type Event struct {
	Type    string
	Seq     int64
	ID      [16]byte
	Replay  bool
	TurnSeq int
	// Baseline is server_seq_baseline of a session.snapshot (0 otherwise).
	Baseline int64
}

// transient events get a server_seq but are never replayed (spec 7.4.3).
var transient = map[string]bool{
	"session.ready": true, "session.snapshot": true, "speech.started": true, "speech.ended": true,
	"stt.partial": true, "ai.text_delta": true, "balance.tick": true, "pong": true,
}

// CheckEvents verifies what a client received over a conversation that included a connection
// drop and a resume (spec 7.4.3, D-083): no event twice (neither by id nor by sequence number), the
// durable events in increasing order of server_seq, every turn that started also completed, and
// the turn numbers without a hole. Gaps in server_seq are normal (transient events) and a
// session.snapshot moves the baseline: the client keeps what it has and continues from there.
func CheckEvents(evs []Event) error {
	if len(evs) == 0 {
		return errors.New("no events received")
	}
	seenSeq := map[int64]bool{}
	seenID := map[[16]byte]bool{}
	var lastDurable int64
	started, completed := map[int]bool{}, map[int]bool{}
	for _, e := range evs {
		if e.Type == "session.snapshot" {
			// the snapshot replaces what the client knows: later durable events continue from its baseline
			lastDurable = max(lastDurable, e.Baseline)
			seenSeq[e.Seq] = true
			continue
		}
		// session.ready belongs to the connection, not to the session: the handshake numbers its own
		// events from 1 (internal/conversation/manager_attach.go), so a resume brings a second one
		// with server_seq 1. It is transient and never replayed; only its event id must be new.
		if e.Type != "session.ready" {
			if seenSeq[e.Seq] {
				return fmt.Errorf("duplicate event: server_seq %d (%s) arrived twice", e.Seq, e.Type)
			}
			seenSeq[e.Seq] = true
		}
		if seenID[e.ID] {
			return fmt.Errorf("duplicate event: id of %s (server_seq %d) arrived twice", e.Type, e.Seq)
		}
		seenID[e.ID] = true
		if transient[e.Type] {
			continue
		}
		if e.Seq <= lastDurable {
			return fmt.Errorf("durable event order: %s (replay=%t) has server_seq %d, not after %d", e.Type, e.Replay, e.Seq, lastDurable)
		}
		lastDurable = e.Seq
		switch e.Type {
		case "turn.started":
			started[e.TurnSeq] = true
		case "turn.completed":
			completed[e.TurnSeq] = true
		}
	}
	turns := make([]int, 0, len(started))
	for t := range started {
		turns = append(turns, t)
	}
	sort.Ints(turns)
	for i, t := range turns {
		if !completed[t] {
			return fmt.Errorf("turn %d started and never completed", t)
		}
		if t != i+1 {
			return fmt.Errorf("turn numbers have a hole: expected turn %d, got turn %d", i+1, t)
		}
	}
	for t := range completed {
		if !started[t] {
			return fmt.Errorf("turn %d completed without turn.started", t)
		}
	}
	return nil
}
