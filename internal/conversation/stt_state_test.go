package conversation

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/provider"
)

// sttClock is the audio time the sttState stamps segments with.
type sttClock struct{ ms int64 }

func (c *sttClock) now() int64 { return c.ms }

func partial(text string) provider.STTEvent {
	return provider.STTEvent{Type: provider.STTPartial, Text: text}
}

func final(text string) provider.STTEvent {
	return provider.STTEvent{Type: provider.STTFinal, Text: text}
}

func commitFinal(text string) provider.STTEvent {
	return provider.STTEvent{Type: provider.STTFinal, Text: text, FromFinalize: true}
}

var _ TextView = (*sttState)(nil)

func TestTurnTextJoinsFinalPlusPartial(t *testing.T) {
	clk := &sttClock{}
	s := newSTTState(clk.now)
	require.Equal(t, FinalSegment, s.OnFinal(final("前の発話。")).Outcome)
	s.MarkTurn(1200)
	require.Equal(t, int64(1200), s.TurnOnsetMs())
	require.Empty(t, s.TurnText(), "segments before the turn mark are not turn text")

	s.OnPartial(partial("昨日は"))
	require.Equal(t, "昨日は", s.TurnText())
	require.Equal(t, FinalSegment, s.OnFinal(final("昨日は雨でした。")).Outcome)
	require.Equal(t, "昨日は雨でした。", s.TurnText(), "the final replaces the running partial")
	s.OnPartial(partial("散歩に"))
	require.Equal(t, "昨日は雨でした。散歩に", s.TurnText())
	s.OnPartial(partial("散歩に行きました"))
	require.Equal(t, "昨日は雨でした。散歩に行きました", s.TurnText(), "a partial overwrites the previous partial")
}

func TestMarkTurnKeepsRunningPartial(t *testing.T) {
	s := newSTTState((&sttClock{}).now)
	s.OnPartial(partial("こん")) // STT is often ahead of SPEECH_CONFIRMED
	s.MarkTurn(0)
	s.OnPartial(partial("こんにちは"))
	require.Equal(t, "こんにちは", s.TurnText())
}

func TestCommitFIFO(t *testing.T) {
	s := newSTTState((&sttClock{}).now)
	s.MarkTurn(0)
	s.OnPartial(partial("こんにちは"))
	te := s.Commit(CommitTurnEnd) // end of the user turn
	dc := s.Commit(CommitDiscard) // entering AI_SPEAKING before the first FINAL arrived
	require.NotEqual(t, te, dc)
	require.Equal(t, 2, s.PendingCommits())

	r := s.OnFinal(commitFinal("こんにちは。"))
	require.Equal(t, FinalResult{Outcome: FinalTurnEnd, Commit: te, Purpose: CommitTurnEnd}, r, "first FINAL answers TURN_END")
	require.Equal(t, "こんにちは。", s.TurnText())
	require.Equal(t, 1, s.CommitIdx())
	require.True(t, s.FinalCoversLatest())

	s.OnPartial(partial("げま")) // echo of the AI voice, audio sent before the DISCARD commit
	r = s.OnFinal(commitFinal("げま"))
	require.Equal(t, FinalResult{Outcome: FinalDiscarded, Commit: dc, Purpose: CommitDiscard}, r, "second FINAL answers DISCARD")
	require.Equal(t, 0, s.PendingCommits())
	require.Equal(t, 1, s.CommitIdx(), "commitIdx moves past the dropped echo")
	require.Equal(t, "こんにちは。", s.TurnText(), "the committed turn keeps its text")

	s.MarkTurn(5000)
	require.Empty(t, s.TurnText(), "the DISCARD text reaches no turn")
	s.OnPartial(partial("次の"))
	require.Equal(t, "次の", s.TurnText())
	require.Equal(t, FinalResult{Outcome: FinalSegment}, s.OnFinal(final("次の質問")), "FINAL without FromFinalize is a plain segment")
	require.Equal(t, 1, s.CommitIdx())
}

func TestCommitDiscardMovesMarkOfTurnStartedWhilePending(t *testing.T) {
	s := newSTTState((&sttClock{}).now)
	s.Commit(CommitDiscard) // back to LISTENING
	s.OnPartial(partial("げま"))
	s.MarkTurn(100) // user turn begins while the DISCARD commit is pending
	require.Equal(t, FinalDiscarded, s.OnFinal(commitFinal("げま")).Outcome)
	require.Equal(t, s.CommitIdx(), s.sttMark, "sttMark = new commitIdx")
	require.Empty(t, s.TurnText())
	s.OnPartial(partial("はい"))
	require.Equal(t, "はい", s.TurnText())
}

func TestTextSinceEmptyWhilePendingDiscard(t *testing.T) {
	clk := &sttClock{}
	s := newSTTState(clk.now)
	s.MarkTurn(0)
	clk.ms = 1000
	s.OnPartial(partial("前の"))
	require.Equal(t, "前の", s.TextSince(0))

	s.Commit(CommitDiscard) // entering AI_SPEAKING
	require.Empty(t, s.TextSince(0), "S-8: \"\" while a DISCARD commit is pending")
	clk.ms = 1500
	s.OnPartial(partial("前のげま")) // old audio, still before the commit
	require.Empty(t, s.TextSince(0))

	clk.ms = 1700
	require.Equal(t, FinalDiscarded, s.OnFinal(commitFinal("前のげま")).Outcome)
	require.Empty(t, s.TextSince(0), "nothing after the commit yet")

	clk.ms = 2000
	s.OnPartial(partial("すみません"))
	require.Equal(t, "すみません", s.TextSince(1800), "only partials after the commit count")
	require.Empty(t, s.TextSince(2100), "segments received before the onset are skipped")

	s.Commit(CommitTurnEnd)
	require.Equal(t, "すみません", s.TextSince(1800), "a pending TURN_END does not blank TextSince")
}

func TestFinalCoversLatestAcrossCommits(t *testing.T) {
	s := newSTTState((&sttClock{}).now)
	s.MarkTurn(0)
	require.False(t, s.FinalCoversLatest(), "no STT event yet")
	s.OnPartial(partial("趣味は何ですか"))
	require.False(t, s.FinalCoversLatest())
	s.OnFinal(final("趣味は何ですか？"))
	require.True(t, s.FinalCoversLatest())
	s.OnPartial(partial("あと"))
	require.False(t, s.FinalCoversLatest(), "a newer partial")
	s.Commit(CommitTurnEnd)
	require.Equal(t, FinalTurnEnd, s.OnFinal(commitFinal("あと")).Outcome)
	require.True(t, s.FinalCoversLatest())

	s.Commit(CommitDiscard)
	require.Equal(t, FinalDiscarded, s.OnFinal(commitFinal("")).Outcome)
	s.MarkTurn(9000)
	require.False(t, s.FinalCoversLatest(), "a new turn starts without events")
}

func TestTimedOutTurnEndCommitDropped(t *testing.T) {
	s := newSTTState((&sttClock{}).now)
	s.MarkTurn(0)
	s.OnFinal(final("おはよう。"))
	s.OnPartial(partial("ございます"))
	id := s.Commit(CommitTurnEnd)
	require.False(t, s.ExpireTurnEnd(id+1), "unknown commit")
	require.True(t, s.ExpireTurnEnd(id), "STT_FINAL_TIMEOUT: the turn text is taken from TurnText")
	require.Equal(t, "おはよう。ございます", s.TurnText())
	require.False(t, s.ExpireTurnEnd(id), "already expired")

	require.Equal(t, FinalTurnEndLate, s.OnFinal(commitFinal("おはようございます")).Outcome)
	require.Equal(t, 0, s.PendingCommits())
	require.Equal(t, 1, s.CommitIdx())
	require.Equal(t, s.CommitIdx(), s.sttMark)
	require.Empty(t, s.TurnText(), "the late FINAL text and the segments before the commit are dropped")
}

func TestOrphanCommitFinalIsSegment(t *testing.T) {
	s := newSTTState((&sttClock{}).now)
	s.MarkTurn(0)
	require.Equal(t, FinalResult{Outcome: FinalSegment}, s.OnFinal(commitFinal("はい")), "FromFinalize without a pending commit")
	require.Equal(t, "はい", s.TurnText())
}

// Row 9 (PROCESSING → SPEECH_CONFIRMED, same turn continues) and the turn then ends again
// before the FINAL of the first TURN_END arrived (review fix, scenario a).
func TestDoubleTurnEndAfterResume(t *testing.T) {
	s := newSTTState((&sttClock{}).now)
	s.MarkTurn(0)
	s.OnPartial(partial("昨日は"))
	id1 := s.Commit(CommitTurnEnd) // SILENCE_REACHED
	require.Equal(t, 1, s.ResumeTurn(), "row 9 supersedes the waiting TURN_END of this turn")
	require.False(t, s.ExpireTurnEnd(id1), "a superseded commit has no timer")
	s.OnPartial(partial("散歩に"))    // speech after the resume
	id2 := s.Commit(CommitTurnEnd) // SILENCE_REACHED again, FINAL#1 still missing
	require.Equal(t, 2, s.PendingCommits())

	r := s.OnFinal(commitFinal("昨日は"))
	require.Equal(t, FinalResult{Outcome: FinalTurnEndSuperseded, Commit: id1, Purpose: CommitTurnEnd}, r,
		"FINAL#1 is not the answer to TURN_END#2")
	require.Equal(t, "昨日は散歩に", s.TurnText(), "FINAL#1 kept as turn text; the resumed partial is not committed away")
	require.False(t, s.FinalCoversLatest(), "the resumed partial is newer than FINAL#1")

	r = s.OnFinal(commitFinal("散歩に行きました。"))
	require.Equal(t, FinalResult{Outcome: FinalTurnEnd, Commit: id2, Purpose: CommitTurnEnd}, r)
	require.Equal(t, "昨日は散歩に行きました。", s.TurnText())
	require.True(t, s.FinalCoversLatest())
	require.Equal(t, 0, s.PendingCommits())
}

func TestResumedTurnEndFinalReplacesPreResumePartial(t *testing.T) {
	s := newSTTState((&sttClock{}).now)
	s.MarkTurn(0)
	s.OnPartial(partial("昨日は"))
	s.Commit(CommitTurnEnd)
	s.OnPartial(partial("昨日は雨")) // late update of audio before the commit
	s.ResumeTurn()
	require.Equal(t, FinalTurnEndSuperseded, s.OnFinal(commitFinal("昨日は雨。")).Outcome)
	require.Equal(t, "昨日は雨。", s.TurnText(), "a partial from before the resume is replaced by the FINAL")
}

// STT_FINAL_TIMEOUT, then row 9 before the first audio (review fix, scenario b): the late FINAL
// must not erase the resumed turn's earlier text.
func TestTimeoutThenResumeKeepsEarlierText(t *testing.T) {
	s := newSTTState((&sttClock{}).now)
	s.MarkTurn(0)
	s.OnFinal(final("昨日は雨でした。"))
	s.OnPartial(partial("散歩"))
	id := s.Commit(CommitTurnEnd)
	require.True(t, s.ExpireTurnEnd(id))
	require.Equal(t, "昨日は雨でした。散歩", s.TurnText(), "text taken at the timeout")
	require.Equal(t, 1, s.ResumeTurn())
	s.OnPartial(partial("それから"))

	r := s.OnFinal(commitFinal("散歩に行きました。"))
	require.Equal(t, FinalResult{Outcome: FinalTurnEndSuperseded, Commit: id, Purpose: CommitTurnEnd}, r)
	require.Equal(t, "昨日は雨でした。散歩に行きました。それから", s.TurnText())
}

func TestResumeTurnIgnoresOtherTurnsAndDiscards(t *testing.T) {
	s := newSTTState((&sttClock{}).now)
	s.MarkTurn(0)
	old := s.Commit(CommitTurnEnd)
	require.True(t, s.ExpireTurnEnd(old)) // its FINAL never came
	s.Commit(CommitDiscard)
	s.MarkTurn(3000) // next user turn
	s.OnPartial(partial("はい"))
	cur := s.Commit(CommitTurnEnd)
	require.Equal(t, 1, s.ResumeTurn(), "only the TURN_END of the current turn")
	require.Equal(t, FinalTurnEndLate, s.OnFinal(commitFinal("古い")).Outcome)
	require.Equal(t, FinalDiscarded, s.OnFinal(commitFinal("げま")).Outcome)
	r := s.OnFinal(commitFinal("はい。"))
	require.Equal(t, cur, r.Commit)
	require.Equal(t, FinalTurnEndSuperseded, r.Outcome)
}
