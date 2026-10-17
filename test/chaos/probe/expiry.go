package probe

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/nabePi/kotoki-backend/internal/conversation/protocol"
	"github.com/nabePi/kotoki-backend/internal/kotokiclient"
)

// Record is what the expiry probe keeps of a server event.
type Record struct {
	Type    string
	Stage   string // session.ending stage: WARNING (T-60 s, T-10 s) or ENDING
	Reason  string // session.ending reason, ai.interrupted reason, session.completed end_reason
	At      time.Duration
	Charged int
}

// CheckExpiry verifies a session that ran out of time while the AI was answering (F15, D-052): it
// went ENDING (stage ENDING, not one of the WARNING notices at T-60 s and T-10 s) with DURATION_ELAPSED
// while an answer was in progress (a generation that started before the ending and a turn.completed,
// or an ai.interrupted SESSION_ENDING, at or after it: without that the probe missed its timing and
// proved nothing), and the session COMPLETED with that end reason, after the ending, charging no more
// than it reserved (the session length). The server lets an answer in progress finish within the
// grace of the ending (at most 10 s) and cuts it only when the grace runs out.
func CheckExpiry(rs []Record, reservedSeconds int) error {
	if len(rs) == 0 {
		return errors.New("no events received")
	}
	var ending, completed *Record
	for i := range rs {
		r := &rs[i]
		switch r.Type {
		case "session.failed":
			return errors.New("the session failed instead of completing")
		case "session.ending":
			if r.Stage == "ENDING" && r.Reason == "DURATION_ELAPSED" && ending == nil {
				ending = r
			}
		case "session.completed":
			completed = r
		}
	}
	if ending == nil {
		return errors.New("no session.ending with stage ENDING and reason DURATION_ELAPSED")
	}
	var started, finishedAfter bool
	for _, r := range rs {
		switch {
		case r.Type == "ai.generation_started" && r.At < ending.At:
			started = true
		case r.Type == "turn.completed" && r.At >= ending.At, r.Type == "ai.interrupted" && r.Reason == "SESSION_ENDING":
			finishedAfter = true
		}
	}
	switch {
	case !started || !finishedAfter:
		return errors.New("the session expired but no answer was in progress (a generation started before the ending that finished or was cut after it): the probe missed its timing, adjust the offset")
	case completed == nil:
		return errors.New("the session never completed after the ending")
	case completed.Reason != "DURATION_ELAPSED":
		return fmt.Errorf("the session completed with end_reason %q, want DURATION_ELAPSED", completed.Reason)
	case completed.At < ending.At:
		return errors.New("session.completed arrived before session.ending")
	case completed.Charged > reservedSeconds:
		return fmt.Errorf("charged %d s, more than the %d s reserved", completed.Charged, reservedSeconds)
	}
	return nil
}

// ExpirySpeaking is the probe of F15: a 300 s session whose user speaks shortly before the session
// runs out, so that the expiry falls while the AI answers.
type ExpirySpeaking struct {
	Common
	// Offset: the second utterance starts this long before the end of the session. The answer to the
	// 1.5 s greeting fixture takes 2.2 s to start and 3.2 s to play (fake provider latencies of the local
	// stack: STT 250, LLM 500, TTS 300 ms), so 4 s puts its generation at 298 s and its audio across
	// the expiry (the answer then finishes within the grace of the ending). Other latencies (staging) need another value; the log shows the real timeline.
	Offset time.Duration
	// FirstTurn is how long the first exchange takes until the AI is done (5.4 s measured on the local
	// stack); the silence that follows it is sized from it.
	FirstTurn time.Duration
}

const expirySessionSeconds = 300

// Run is the Probe function of ExpirySpeaking.
func (p ExpirySpeaking) Run(ctx context.Context, _ []string) error {
	offset, first := p.Offset, p.FirstTurn
	if offset <= 0 {
		offset = 4 * time.Second
	}
	if first <= 0 {
		first = 5500 * time.Millisecond
	}
	pk, err := p.speech("ja_greeting_short")
	if err != nil {
		return err
	}
	c, sess, err := p.open(ctx, expirySessionSeconds)
	if err != nil {
		return err
	}
	var (
		mu sync.Mutex
		rs []Record
	)
	t0 := time.Now()
	sink := kotokiclient.SinkFuncs{Event: func(r kotokiclient.Received[protocol.ServerEvent]) {
		rec := Record{Type: r.Value.Type, At: r.At.Sub(t0)}
		switch pl := r.Value.Payload.(type) {
		case protocol.SessionEnding:
			rec.Stage, rec.Reason = pl.Stage, pl.Reason
		case protocol.AIInterrupted:
			rec.Reason = pl.Reason
		case protocol.SessionCompleted:
			rec.Reason, rec.Charged = pl.EndReason, pl.ChargedSeconds
		}
		mu.Lock()
		rs = append(rs, rec)
		mu.Unlock()
	}}
	wait := expirySessionSeconds*time.Second - offset - first
	steps := []kotokiclient.Step{
		kotokiclient.Speak{Packets: pk, Label: "first"},
		kotokiclient.WaitAIDone{Timeout: 30 * time.Second},
		kotokiclient.Think{D: wait},
		kotokiclient.Speak{Packets: pk, Label: "near the end"},
		kotokiclient.Think{D: offset + 30*time.Second}, // the server ends the session; the margin lets it finish
	}
	rep, err := kotokiclient.RunConversation(ctx, c, kotokiclient.SessionSpec{
		SessionID: sess.ID, WSURL: p.WSURL, Origin: p.Origin,
		Client: &protocol.ClientInfo{Platform: "chaos", AppVersion: "kotoki-chaos"},
	}, steps, sink)
	mu.Lock()
	defer mu.Unlock()
	for _, r := range rs {
		switch r.Type {
		case "session.started", "turn.started", "ai.generation_started", "session.ending", "ai.interrupted", "session.completed", "turn.completed":
			p.logf("F15 probe: +%6.1fs %-22s %s %s", r.At.Seconds(), r.Type, r.Stage, r.Reason)
		}
	}
	if err != nil {
		return fmt.Errorf("probe conversation (session %s): %w", sess.ID, err)
	}
	p.logf("F15 probe: session %s, status=%s, charged=%ds", sess.ID, rep.Status, rep.ChargedSeconds)
	if err := CheckExpiry(rs, expirySessionSeconds); err != nil {
		return fmt.Errorf("probe: session %s: %w", sess.ID, err)
	}
	return nil
}
