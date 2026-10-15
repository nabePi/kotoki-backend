package conversation

import (
	"strings"

	"github.com/nabePi/kotoki-backend/internal/provider"
)

// CommitPurpose is why the runtime called stt.Finalize (S-8, spec 7.6).
type CommitPurpose string

// Commit purposes: (a) end of the user turn; (b) entering AI_SPEAKING and (c) back to
// LISTENING (commitBoundary).
const (
	CommitTurnEnd CommitPurpose = "TURN_END"
	CommitDiscard CommitPurpose = "DISCARD"
)

// CommitID identifies one Commit (one stt.Finalize call); 0 is "no commit".
type CommitID uint64

// FinalOutcome tells Runtime.loop what an STT FINAL meant.
type FinalOutcome int

// FINAL outcomes.
const (
	// FinalSegment: a FINAL{FromFinalize:false} (or a commit FINAL with no commit pending)
	// added a final segment.
	FinalSegment FinalOutcome = iota
	// FinalTurnEnd: the FINAL of the TURN_END commit the turn is waiting for was added →
	// STT_COMMITTED_FINAL (row 7/8).
	FinalTurnEnd
	// FinalTurnEndLate: the FINAL of a TURN_END commit whose STT_FINAL_TIMEOUT fired (the turn
	// text was taken from TurnText); the text and the segments before the commit are dropped.
	FinalTurnEndLate
	// FinalTurnEndSuperseded: the FINAL of a TURN_END commit whose turn resumed (row 9,
	// ResumeTurn). The turn continues, so the text is kept as a final segment of that turn
	// (sttMark unchanged); it is NOT STT_COMMITTED_FINAL.
	FinalTurnEndSuperseded
	// FinalDiscarded: the FINAL of a DISCARD commit; dropped with everything before it
	// (metric stt_discarded_commits_total).
	FinalDiscarded
)

// FinalResult is what OnFinal did: the outcome and, for a commit FINAL, which commit it
// answered (Commit/Purpose are zero for FinalSegment).
type FinalResult struct {
	Outcome FinalOutcome
	Commit  CommitID
	Purpose CommitPurpose
}

// Segment is one STT segment: final, or the running partial (only ever the last one).
// RecvAudioMs is the audio time when it was (last) received.
type Segment struct {
	Text        string
	Final       bool
	RecvAudioMs int64
}

// pendingCommit is one stt.Finalize call still waiting for its FINAL{FromFinalize:true}. turn
// is the user turn (MarkTurn count) that was current at the Commit: the spec's per-entry
// turnID, so "is the turn of p still waiting" can be answered.
type pendingCommit struct {
	id         CommitID
	purpose    CommitPurpose
	turn       uint64
	expired    bool   // TURN_END whose STT_FINAL_TIMEOUT fired
	superseded bool   // TURN_END whose turn resumed (row 9)
	resumeGen  uint64 // partialGen at ResumeTurn: later partials are speech after the resume
}

// sttState is the STT text of the session as Runtime.loop sees it (spec 7.6.2): the
// segments, the FIFO of pending commits (one per stt.Finalize, S-8), commitIdx and the turn
// mark sttMark. It implements TextView. Time is audio time (audioNow), never a clock. Only
// Runtime.loop uses it, so it is not safe for concurrent use.
//
// Hand-off for Runtime.loop (Tasks 19/21):
//   - Every stt.Finalize goes with exactly one Commit; keep the returned CommitID of a
//     TURN_END for its STT_FINAL_TIMEOUT timer and call ExpireTurnEnd(id) when it fires.
//   - Row 9 (PROCESSING → SPEECH_CONFIRMED, same turn continues): call ResumeTurn() and stop
//     the STT timer. Its FINAL then yields FinalTurnEndSuperseded (text kept in the turn) and
//     never STT_COMMITTED_FINAL; the turn ends again with a new TURN_END commit, and only the
//     FINAL whose Commit equals that new id is FinalTurnEnd. Scenario (a), double end before
//     FINAL#1: FINAL#1 → Superseded{id1}, FINAL#2 → TurnEnd{id2}. Scenario (b), timeout then
//     resume: the resume wins over the timeout, the late FINAL → Superseded and the earlier
//     text of the turn survives.
//   - Act on STT_COMMITTED_FINAL only for FinalTurnEnd; FinalTurnEndLate and FinalDiscarded
//     need no action besides metrics.
type sttState struct {
	audioNow   func() int64
	segments   []Segment
	pending    []pendingCommit
	nextID     CommitID // id of the next Commit (ids start at 1)
	markID     CommitID // nextID at MarkTurn: commits with id < markID were pending at the turn start
	turn       uint64   // MarkTurn count
	partialGen uint64   // OnPartial count; the running partial is the partialGen-th
	commitIdx  int      // first segment after the FINAL of the last answered commit
	sttMark    int      // first segment of the current user turn; sttMark >= commitIdx
	onsetMs    int64
}

// newSTTState returns an empty state stamping segments with audioNow().
func newSTTState(audioNow func() int64) *sttState {
	return &sttState{audioNow: audioNow, nextID: 1}
}

// hasPartial reports whether the last segment is the running partial.
func (s *sttState) hasPartial() bool {
	n := len(s.segments)
	return n > 0 && !s.segments[n-1].Final
}

func (s *sttState) dropPartial() {
	if s.hasPartial() {
		s.segments = s.segments[:len(s.segments)-1]
	}
}

// OnPartial records the running segment text; it replaces the previous partial.
func (s *sttState) OnPartial(ev provider.STTEvent) {
	s.dropPartial()
	s.partialGen++
	s.segments = append(s.segments, Segment{Text: ev.Text, RecvAudioMs: s.audioNow()})
}

// appendFinal closes the running segment with text (which may be empty).
func (s *sttState) appendFinal(text string) {
	s.dropPartial()
	s.segments = append(s.segments, Segment{Text: text, Final: true, RecvAudioMs: s.audioNow()})
}

// OnFinal handles a FINAL. A FINAL{FromFinalize:true} answers the oldest pending commit
// (FIFO; the adapter guarantees exactly one per Finalize, in order) and moves commitIdx past
// it; the running partial is audio from before that commit and goes with it, except a
// partial received after ResumeTurn, which is speech of the resumed turn and stays after the
// FINAL.
func (s *sttState) OnFinal(ev provider.STTEvent) FinalResult {
	if !ev.FromFinalize || len(s.pending) == 0 {
		s.appendFinal(ev.Text)
		return FinalResult{Outcome: FinalSegment}
	}
	p := s.pending[0]
	s.pending = s.pending[1:]
	res := FinalResult{Commit: p.id, Purpose: p.purpose}
	switch {
	case p.purpose == CommitTurnEnd && p.superseded:
		// The turn resumed: keep the text in the turn (sttMark unchanged), before any partial
		// of the resumed speech.
		seg := Segment{Text: ev.Text, Final: true, RecvAudioMs: s.audioNow()}
		if s.hasPartial() && s.partialGen > p.resumeGen {
			n := len(s.segments)
			s.segments = append(s.segments[:n-1], seg, s.segments[n-1])
			s.commitIdx = n
		} else {
			s.dropPartial()
			s.segments = append(s.segments, seg)
			s.commitIdx = len(s.segments)
		}
		res.Outcome = FinalTurnEndSuperseded
	case p.purpose == CommitTurnEnd && !p.expired:
		// The segments stay with the waiting turn (sttMark unchanged).
		s.appendFinal(ev.Text)
		s.commitIdx = len(s.segments)
		res.Outcome = FinalTurnEnd
	case p.expired:
		// The turn text was already taken from TurnText; drop the late text and the segments.
		s.dropPartial()
		s.commitIdx = len(s.segments)
		s.sttMark = max(s.sttMark, s.commitIdx)
		res.Outcome = FinalTurnEndLate
	default:
		s.dropPartial()
		s.commitIdx = len(s.segments)
		if p.id < s.markID {
			// A user turn that began while this DISCARD commit was pending starts after it.
			s.sttMark = max(s.sttMark, s.commitIdx)
		}
		res.Outcome = FinalDiscarded
	}
	return res
}

// Commit records one stt.Finalize call (the caller calls Finalize itself) for the current
// turn and returns its id.
func (s *sttState) Commit(purpose CommitPurpose) CommitID {
	id := s.nextID
	s.nextID++
	s.pending = append(s.pending, pendingCommit{id: id, purpose: purpose, turn: s.turn})
	return id
}

// ExpireTurnEnd marks the TURN_END commit id as timed out (STT_FINAL_TIMEOUT), so its FINAL
// is dropped when it arrives; false when id is not a waiting TURN_END (unknown, answered,
// already expired or superseded).
func (s *sttState) ExpireTurnEnd(id CommitID) bool {
	for i := range s.pending {
		if p := &s.pending[i]; p.id == id {
			if p.purpose != CommitTurnEnd || p.expired || p.superseded {
				return false
			}
			p.expired = true
			return true
		}
	}
	return false
}

// ResumeTurn handles row 9 (the user resumes in PROCESSING, the same turn continues): every
// pending TURN_END commit of the current turn, expired or not, is superseded, so its FINAL is
// kept as turn text (FinalTurnEndSuperseded). It returns how many commits it superseded.
func (s *sttState) ResumeTurn() int {
	n := 0
	for i := range s.pending {
		if p := &s.pending[i]; p.purpose == CommitTurnEnd && p.turn == s.turn && !p.superseded {
			p.superseded, p.resumeGen = true, s.partialGen
			n++
		}
	}
	return n
}

// PendingCommits is the number of Finalize calls still waiting for their FINAL.
func (s *sttState) PendingCommits() int { return len(s.pending) }

// MarkTurn starts a user turn at onsetMs (beginUserTurn): the turn text begins with the
// running partial, if any, since STT usually runs ahead of SPEECH_CONFIRMED.
func (s *sttState) MarkTurn(onsetMs int64) {
	mark := len(s.segments)
	if s.hasPartial() {
		mark--
	}
	s.sttMark = max(mark, s.commitIdx)
	s.markID = s.nextID
	s.turn++
	s.onsetMs = onsetMs
}

// CommitIdx is the first segment after the FINAL of the last answered commit.
func (s *sttState) CommitIdx() int { return s.commitIdx }

// TurnOnsetMs is the onset given to the last MarkTurn.
func (s *sttState) TurnOnsetMs() int64 { return s.onsetMs }

// TurnText joins the final segments since sttMark and the running partial.
func (s *sttState) TurnText() string {
	var b strings.Builder
	for _, seg := range s.segments[s.sttMark:] {
		b.WriteString(seg.Text)
	}
	return b.String()
}

// TextSince joins the segments after commitIdx received at audio time >= onsetMs; it is ""
// while a DISCARD commit is pending, because the partial then is audio from before the commit
// (S-8: an echo must not confirm a barge-in).
func (s *sttState) TextSince(onsetMs int64) string {
	for _, p := range s.pending {
		if p.purpose == CommitDiscard {
			return ""
		}
	}
	var b strings.Builder
	for _, seg := range s.segments[s.commitIdx:] {
		if seg.RecvAudioMs >= onsetMs {
			b.WriteString(seg.Text)
		}
	}
	return b.String()
}

// FinalCoversLatest reports that the latest STT event of the turn is a final (D-076).
func (s *sttState) FinalCoversLatest() bool {
	n := len(s.segments)
	return n > s.sttMark && s.segments[n-1].Final
}
