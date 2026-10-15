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

// FinalOutcome tells Runtime.loop what an STT FINAL meant.
type FinalOutcome int

// FINAL outcomes.
const (
	// FinalSegment: a FINAL{FromFinalize:false} (or a commit FINAL with no commit pending)
	// added a final segment.
	FinalSegment FinalOutcome = iota
	// FinalTurnEnd: the FINAL of a waiting TURN_END commit was added → STT_COMMITTED_FINAL.
	FinalTurnEnd
	// FinalTurnEndLate: the FINAL of a TURN_END commit that already timed out; dropped.
	FinalTurnEndLate
	// FinalDiscarded: the FINAL of a DISCARD commit; dropped with everything before it
	// (metric stt_discarded_commits_total).
	FinalDiscarded
)

// Segment is one STT segment: final, or the running partial (only ever the last one).
// RecvAudioMs is the audio time when it was (last) received.
type Segment struct {
	Text        string
	Final       bool
	RecvAudioMs int64
}

// pendingCommit is one stt.Finalize call still waiting for its FINAL{FromFinalize:true}.
type pendingCommit struct {
	purpose CommitPurpose
	seq     uint64 // order of the Commit call
	expired bool   // TURN_END whose STT_FINAL_TIMEOUT fired
}

// sttState is the STT text of the session as Runtime.loop sees it (spec 7.6.2): the
// segments, the FIFO of pending commits (one per stt.Finalize, S-8), commitIdx and the turn
// mark sttMark. It implements TextView. Time is audio time (audioNow), never a clock. Only
// Runtime.loop uses it, so it is not safe for concurrent use.
type sttState struct {
	audioNow  func() int64
	segments  []Segment
	pending   []pendingCommit
	nextSeq   uint64 // seq of the next Commit
	markSeq   uint64 // nextSeq at MarkTurn: commits with seq < markSeq were pending at the turn start
	commitIdx int    // first segment after the FINAL of the last answered commit
	sttMark   int    // first segment of the current user turn; sttMark >= commitIdx
	onsetMs   int64
}

// newSTTState returns an empty state stamping segments with audioNow().
func newSTTState(audioNow func() int64) *sttState {
	return &sttState{audioNow: audioNow}
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
	s.segments = append(s.segments, Segment{Text: ev.Text, RecvAudioMs: s.audioNow()})
}

// appendFinal closes the running segment with text (which may be empty).
func (s *sttState) appendFinal(text string) {
	s.dropPartial()
	s.segments = append(s.segments, Segment{Text: text, Final: true, RecvAudioMs: s.audioNow()})
}

// OnFinal handles a FINAL. A FINAL{FromFinalize:true} answers the oldest pending commit
// (FIFO; the adapter guarantees exactly one per Finalize, in order) and moves commitIdx past
// it; the running partial is audio from before that commit and goes with it.
func (s *sttState) OnFinal(ev provider.STTEvent) FinalOutcome {
	if !ev.FromFinalize || len(s.pending) == 0 {
		s.appendFinal(ev.Text)
		return FinalSegment
	}
	p := s.pending[0]
	s.pending = s.pending[1:]
	if p.purpose == CommitTurnEnd && !p.expired {
		// The segments stay with the waiting turn (sttMark unchanged).
		s.appendFinal(ev.Text)
		s.commitIdx = len(s.segments)
		return FinalTurnEnd
	}
	s.dropPartial()
	s.commitIdx = len(s.segments)
	if p.expired {
		// The turn text was already taken from TurnText; drop the late text and the segments.
		s.sttMark = max(s.sttMark, s.commitIdx)
		return FinalTurnEndLate
	}
	if p.seq < s.markSeq {
		// A user turn that began while this DISCARD commit was pending starts after it.
		s.sttMark = max(s.sttMark, s.commitIdx)
	}
	return FinalDiscarded
}

// Commit records one stt.Finalize call; the caller calls Finalize itself.
func (s *sttState) Commit(purpose CommitPurpose) {
	s.pending = append(s.pending, pendingCommit{purpose: purpose, seq: s.nextSeq})
	s.nextSeq++
}

// ExpireTurnEnd marks the oldest waiting TURN_END commit as timed out (STT_FINAL_TIMEOUT), so
// its FINAL is dropped when it arrives; false when no TURN_END is waiting.
func (s *sttState) ExpireTurnEnd() bool {
	for i := range s.pending {
		if p := &s.pending[i]; p.purpose == CommitTurnEnd && !p.expired {
			p.expired = true
			return true
		}
	}
	return false
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
	s.markSeq = s.nextSeq
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
