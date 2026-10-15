package conversation

import (
	"time"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/conversation/protocol"
	"github.com/nabePi/kotoki-backend/internal/session"
)

// hintWindow is how long after a client.barge_in hint a server barge-in still confirms it
// (§7.14 bargein_total).
const hintWindow = time.Second

// maxHints bounds the hints waiting for a classification (the reader's JSON limit is 20/s).
const maxHints = 64

// Sources of bargein_total.
const (
	bargeSourceServer          = "server"
	bargeSourceHintConfirmed   = "hint_confirmed"
	bargeSourceHintUnconfirmed = "hint_unconfirmed"
)

// pendingBargeStop is a barge-in waiting for its stop time: when it was confirmed, which turn and
// generation it cut and how many sentences had been heard.
type pendingBargeStop struct {
	from   time.Time
	turnID uuid.UUID
	gen    uint32
	spoken int
}

// onBargeInConfirmed is row 13: the detector confirmed a barge-in (≥ 300 ms voiced in 400 ms and a
// partial of ≥ 2 runes since the candidate onset, D-077). It only applies while the AI speaks and
// turns are accepted (row 13 guard); the detector reports it for every candidate, so an event in
// any other state is dropped here.
func (r *Runtime) onBargeInConfirmed(onsetMs int64) {
	if !r.accepting || r.turnState != StateAISpeaking || r.turn == nil || r.gen == nil {
		return
	}
	r.bargeIn(onsetMs)
}

// bargeIn is the procedure of §7.6.5: the generation is cancelled (generation++, the writer's
// active generation, the queued frames purged), the client hears ai.interrupted, the turn that was
// speaking is finalized and the user's turn begins from the onset of the barge-in. Nothing here
// waits for the goroutines of the old generation; the events they still send are dropped by
// onGenEvent (ev.Gen != r.gen.number).
func (r *Runtime) bargeIn(onsetMs int64) {
	t0 := r.d.Clock.Now()
	t, g := r.turn, r.gen
	spoken := r.interruptGeneration("BARGE_IN") // cancel, generation++, PurgeAudio, ai.interrupted{BARGE_IN}
	r.d.Metrics.BargeIn(bargeSourceServer)
	r.confirmHints(t0)
	r.bargeStop = &pendingBargeStop{from: t0, turnID: t.id, gen: g.number, spoken: spoken}

	if t.failed != nil { // an APOLOGY was cut: the turn stays FAILED, not INTERRUPTED (§7.6.5)
		r.completeFailedTurn(false)
	} else {
		text, spokenText := g.g.Text(), g.spokenText(spoken)
		r.submit(FinalizeTurn{TurnID: t.id, Final: r.finalOf(g, session.TurnInterrupted, spokenText)})
		r.d.Metrics.Turn(string(session.TurnInterrupted))
		tc := protocol.TurnCompleted{Status: string(session.TurnInterrupted), AssistantText: &text, AssistantSpokenText: &spokenText}
		if t.hasUser {
			u := t.userText
			tc.UserText = &u
		}
		r.emit("turn.completed", evMeta{turn: t}, tc)
		r.turn = nil
		r.sync.ClearTurn()
		r.sttAudioBase = r.sttAudioTotal()
		r.postTurn(t, session.TurnInterrupted, spokenText, spokenText)
	}
	r.beginUserTurn(onsetMs, true) // marks the STT text from the onset, speech.started{during_ai_speech}, turn.started
}

// settleBargeIn observes bargein_stop_seconds for the last barge-in (D-101) and writes its BARGE_IN
// session event: the stop time is from its confirmation to the last audio frame the writer wrote.
// After PurgeAudio the writer is at most inside one write and drops every later frame of the old
// generation, so the stamp is final by the next tick.
func (r *Runtime) settleBargeIn(now time.Time) {
	r.expireHints(now)
	b := r.bargeStop
	if b == nil {
		return
	}
	r.bargeStop = nil
	var d time.Duration
	if r.cs != nil {
		d = max(0, r.cs.writer.LastAudioWrite().Sub(b.from))
	}
	r.d.Metrics.BargeInStop(d)
	id := b.turnID
	r.submit(SessionEvent{Type: string(session.EventBargeIn), TurnID: &id, Payload: map[string]int64{
		"generation": int64(b.gen), "spoken_sentence_count": int64(b.spoken), "stop_ms": d.Milliseconds(),
	}})
}

// onBargeInHint is row 18: client.barge_in is a hint and changes no state. It is classified later:
// hint_confirmed when a server barge-in follows within hintWindow, hint_unconfirmed otherwise.
func (r *Runtime) onBargeInHint() {
	now := r.d.Clock.Now()
	r.expireHints(now)
	if len(r.hints) >= maxHints {
		r.d.Metrics.BargeIn(bargeSourceHintUnconfirmed)
		r.hints = r.hints[1:]
	}
	r.hints = append(r.hints, now)
}

// confirmHints classifies the hints still within the window as confirmed by the barge-in at now.
func (r *Runtime) confirmHints(now time.Time) {
	r.expireHints(now)
	for range r.hints {
		r.d.Metrics.BargeIn(bargeSourceHintConfirmed)
	}
	r.hints = r.hints[:0]
}

// expireHints classifies the hints older than the window as unconfirmed.
func (r *Runtime) expireHints(now time.Time) {
	n := 0
	for n < len(r.hints) && now.Sub(r.hints[n]) > hintWindow {
		r.d.Metrics.BargeIn(bargeSourceHintUnconfirmed)
		n++
	}
	r.hints = r.hints[n:]
}
