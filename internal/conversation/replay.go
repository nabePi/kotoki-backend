package conversation

import (
	"encoding/json"
	"fmt"

	"github.com/nabePi/kotoki-backend/internal/conversation/rtstate"
)

// Values of session.snapshot.replay (§7.4.5).
const (
	replayComplete    = "COMPLETE"
	replayUnavailable = "UNAVAILABLE"
)

// replayMaxEvents bounds what one resume sends back: the replayed events are durable control
// messages, and a client that sends more than half of the outbound queue (WS_OUTBOUND_QUEUE, 256)
// at once would be closed as a slow consumer. A longer gap is answered with the snapshot alone
// (replay UNAVAILABLE): the client rebuilds the screen from recent_turns.
const replayMaxEvents = 128

// replay is the algorithm of §7.10.4. entries are the events of the replay stream in server_seq
// order (nil when the stream is empty or could not be read), lastSeq the largest server_seq the
// client processed and knownLast the largest server_seq this session has issued. It returns the
// durable events the client missed (to be sent with replay=true) and the mode of session.snapshot:
// COMPLETE when nothing the client missed is gone, UNAVAILABLE when it cannot be told. Every
// entry carries prev_seq, the server_seq of the durable event before it, so an event that was
// trimmed from the stream (or lost while Redis was down) shows up as a gap: the first missed
// entry's prev_seq is beyond what the client has.
func replay(entries []rtstate.EventEntry, lastSeq, knownLast int64) ([]rtstate.EventEntry, string) {
	if len(entries) == 0 {
		if lastSeq == knownLast {
			return nil, replayComplete // nothing was missed
		}
		return nil, replayUnavailable
	}
	if lastSeq > knownLast {
		return nil, replayUnavailable // a claim the server cannot have caused
	}
	var missed []rtstate.EventEntry
	for _, e := range entries {
		if e.Seq > lastSeq {
			missed = append(missed, e)
		}
	}
	if len(missed) == 0 {
		return nil, replayComplete
	}
	if missed[0].PrevSeq <= lastSeq { // no durable event between the client's last one and the first missed
		return missed, replayComplete
	}
	return nil, replayUnavailable
}

// withReplayFlag returns the envelope with "replay": true; every other field, the server_seq
// included, is the one the event had when it was first sent.
func withReplayFlag(env []byte) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(env, &m); err != nil {
		return nil, fmt.Errorf("conversation: decode replay entry: %w", err)
	}
	m["replay"] = json.RawMessage("true")
	out, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("conversation: encode replay entry: %w", err)
	}
	return out, nil
}
