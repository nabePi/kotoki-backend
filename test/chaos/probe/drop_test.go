package probe_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/test/chaos/probe"
)

func ev(typ string, seq int64, replay bool, turn int) probe.Event {
	return probe.Event{Type: typ, Seq: seq, ID: uuid.New(), Replay: replay, TurnSeq: turn}
}

// a clean conversation with a drop: the events after the drop are replays of what the client had
// not yet seen, the rest of the sequence continues
func cleanRun() []probe.Event {
	return []probe.Event{
		ev("session.ready", 1, false, 0), ev("session.started", 2, false, 0),
		ev("turn.started", 3, false, 1), ev("stt.partial", 4, false, 1), ev("stt.final", 5, false, 1),
		ev("ai.generation_started", 6, false, 1), ev("ai.sentence", 7, false, 1), ev("turn.completed", 8, false, 1),
		// drop; the server replays the durable events after last_server_seq=8: none; the new turn follows
		ev("turn.started", 9, false, 2), ev("stt.final", 10, false, 2), ev("turn.completed", 11, false, 2),
	}
}

func TestCheckEvents_CleanRunPasses(t *testing.T) {
	require.NoError(t, probe.CheckEvents(cleanRun()))
}

func TestCheckEvents_ReplayFillsTheGap(t *testing.T) {
	evs := cleanRun()
	// events 9 and 10 were emitted while the client was away; the resume replays the durable one (10)
	// and the transient one (9) is a gap, which is normal (spec 7.4.3)
	evs = append(evs[:8:8], ev("stt.final", 10, true, 2), ev("turn.completed", 11, true, 2))
	evs = append(evs[:8], append([]probe.Event{ev("turn.started", 9, true, 2)}, evs[8:]...)...)
	require.NoError(t, probe.CheckEvents(evs))
}

func TestCheckEvents_EachConnectionHasItsOwnSessionReady(t *testing.T) {
	evs := cleanRun()
	again := ev("session.ready", 1, false, 0) // the handshake of the resumed connection numbers from 1 again
	evs = append(evs[:8:8], append([]probe.Event{again}, evs[8:]...)...)
	require.NoError(t, probe.CheckEvents(evs))

	same := evs[0]
	same.Seq = 1
	require.ErrorContains(t, probe.CheckEvents(append(evs, same)), "duplicate", "but the same event id twice is still a duplicate")
}

func TestCheckEvents_Duplicates(t *testing.T) {
	evs := cleanRun()
	dup := evs[7]
	dup.Replay = true
	require.ErrorContains(t, probe.CheckEvents(append(evs, dup)), "duplicate", "the same event twice")

	evs = cleanRun()
	same := ev("turn.completed", 8, true, 1) // a different event id on a sequence number already seen
	require.ErrorContains(t, probe.CheckEvents(append(evs, same)), "duplicate")
}

func TestCheckEvents_SameEventIdOnADifferentSequenceNumber(t *testing.T) {
	evs := cleanRun()
	again := evs[7]
	again.Seq, again.Replay = 12, true // the same event (id) delivered once more under a new server_seq
	require.ErrorContains(t, probe.CheckEvents(append(evs, again)), "duplicate")
}

func TestCheckEvents_DurableEventBelowTheSnapshotBaselineIsStale(t *testing.T) {
	evs := cleanRun()
	snap := ev("session.snapshot", 20, false, 0)
	snap.Baseline = 30
	evs = append(evs, snap, ev("turn.started", 25, false, 3)) // below the baseline the client adopted
	require.ErrorContains(t, probe.CheckEvents(evs), "order")
}

func TestCheckEvents_DurableOrderMustNotGoBack(t *testing.T) {
	evs := cleanRun()
	evs[8], evs[7] = evs[7], evs[8]
	require.ErrorContains(t, probe.CheckEvents(evs), "order")
}

func TestCheckEvents_TurnWithoutCompletion(t *testing.T) {
	evs := cleanRun()[:10] // turn 2 started, never completed
	require.ErrorContains(t, probe.CheckEvents(evs), "turn 2")
}

func TestCheckEvents_MissingTurnNumber(t *testing.T) {
	evs := cleanRun()
	evs[2].TurnSeq, evs[3].TurnSeq, evs[4].TurnSeq, evs[5].TurnSeq, evs[6].TurnSeq, evs[7].TurnSeq = 1, 1, 1, 1, 1, 1
	for i := 8; i < len(evs); i++ {
		evs[i].TurnSeq = 3 // turn 2 lost
	}
	require.ErrorContains(t, probe.CheckEvents(evs), "turn")
}

func TestCheckEvents_SnapshotResetsTheBaseline(t *testing.T) {
	evs := cleanRun()
	snap := ev("session.snapshot", 20, false, 0)
	snap.Baseline = 30
	evs = append(evs, snap, ev("turn.started", 31, false, 3), ev("turn.completed", 32, false, 3))
	require.NoError(t, probe.CheckEvents(evs))
}

func TestCheckEvents_Empty(t *testing.T) {
	require.Error(t, probe.CheckEvents(nil))
}
