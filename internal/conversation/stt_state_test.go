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
	require.Equal(t, FinalSegment, s.OnFinal(final("前の発話。")))
	s.MarkTurn(1200)
	require.Equal(t, int64(1200), s.TurnOnsetMs())
	require.Empty(t, s.TurnText(), "segments before the turn mark are not turn text")

	s.OnPartial(partial("昨日は"))
	require.Equal(t, "昨日は", s.TurnText())
	require.Equal(t, FinalSegment, s.OnFinal(final("昨日は雨でした。")))
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
	s.Commit(CommitTurnEnd) // end of the user turn
	s.Commit(CommitDiscard) // entering AI_SPEAKING before the first FINAL arrived
	require.Equal(t, 2, s.PendingCommits())

	require.Equal(t, FinalTurnEnd, s.OnFinal(commitFinal("こんにちは。")), "first FINAL answers TURN_END")
	require.Equal(t, "こんにちは。", s.TurnText())
	require.Equal(t, 1, s.CommitIdx())
	require.True(t, s.FinalCoversLatest())

	s.OnPartial(partial("げま")) // echo of the AI voice, audio sent before the DISCARD commit
	require.Equal(t, FinalDiscarded, s.OnFinal(commitFinal("げま")), "second FINAL answers DISCARD")
	require.Equal(t, 0, s.PendingCommits())
	require.Equal(t, 1, s.CommitIdx(), "commitIdx moves past the dropped echo")
	require.Equal(t, "こんにちは。", s.TurnText(), "the committed turn keeps its text")

	s.MarkTurn(5000)
	require.Empty(t, s.TurnText(), "the DISCARD text reaches no turn")
	s.OnPartial(partial("次の"))
	require.Equal(t, "次の", s.TurnText())
	require.Equal(t, FinalSegment, s.OnFinal(final("次の質問")), "FINAL without FromFinalize is a plain segment")
	require.Equal(t, 1, s.CommitIdx())
}

func TestCommitDiscardMovesMarkOfTurnStartedWhilePending(t *testing.T) {
	s := newSTTState((&sttClock{}).now)
	s.Commit(CommitDiscard) // back to LISTENING
	s.OnPartial(partial("げま"))
	s.MarkTurn(100) // user turn begins while the DISCARD commit is pending
	require.Equal(t, FinalDiscarded, s.OnFinal(commitFinal("げま")))
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
	require.Equal(t, FinalDiscarded, s.OnFinal(commitFinal("前のげま")))
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
	require.Equal(t, FinalTurnEnd, s.OnFinal(commitFinal("あと")))
	require.True(t, s.FinalCoversLatest())

	s.Commit(CommitDiscard)
	require.Equal(t, FinalDiscarded, s.OnFinal(commitFinal("")))
	s.MarkTurn(9000)
	require.False(t, s.FinalCoversLatest(), "a new turn starts without events")
}

func TestTimedOutTurnEndCommitDropped(t *testing.T) {
	s := newSTTState((&sttClock{}).now)
	s.MarkTurn(0)
	s.OnFinal(final("おはよう。"))
	s.OnPartial(partial("ございます"))
	s.Commit(CommitTurnEnd)
	require.True(t, s.ExpireTurnEnd(), "STT_FINAL_TIMEOUT: the turn text is taken from TurnText")
	require.Equal(t, "おはよう。ございます", s.TurnText())
	require.False(t, s.ExpireTurnEnd(), "no other TURN_END is waiting")

	require.Equal(t, FinalTurnEndLate, s.OnFinal(commitFinal("おはようございます")))
	require.Equal(t, 0, s.PendingCommits())
	require.Equal(t, 1, s.CommitIdx())
	require.Equal(t, s.CommitIdx(), s.sttMark)
	require.Empty(t, s.TurnText(), "the late FINAL text and the segments before the commit are dropped")
}

func TestOrphanCommitFinalIsSegment(t *testing.T) {
	s := newSTTState((&sttClock{}).now)
	s.MarkTurn(0)
	require.Equal(t, FinalSegment, s.OnFinal(commitFinal("はい")), "FromFinalize without a pending commit")
	require.Equal(t, "はい", s.TurnText())
}
