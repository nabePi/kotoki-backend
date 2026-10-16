//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/conversation/protocol"
	"github.com/nabePi/kotoki-backend/internal/kotokiclient"
)

// TestE3BargeIn is E3 (spec 14.5): the user speaks over the AI one second after its first audio
// frame. The server confirms the barge-in, stops the AI at once and keeps the part that was heard.
func TestE3BargeIn(t *testing.T) {
	u := newUser(t)
	s := startSession(t, u, "USER_FIRST")
	rec := newRecorder()
	rep, err := converse(t, u, s, []kotokiclient.Step{
		kotokiclient.Speak{Packets: packets(t, "ja_question"), Label: "ja_question"},
		kotokiclient.BargeIn{After: time.Second, Packets: packets(t, "ja_barge_in")},
		kotokiclient.WaitAIDone{},
		kotokiclient.End{},
	}, rec)
	require.NoError(t, err)
	require.Equal(t, "COMPLETED", rep.Status)

	inOrder(t, rec.types(), []string{"ai.audio_started", "ai.interrupted", "turn.completed", "speech.started", "turn.started"})
	ints := rec.of("ai.interrupted")
	require.Len(t, ints, 1)
	ip := ints[0].Payload.(protocol.AIInterrupted)
	assert.Equal(t, "BARGE_IN", ip.Reason)

	// Nothing of the interrupted generation reaches the client once the server says it was
	// interrupted: the client discards (and counts) frames of an older generation, so any such
	// frame would show up here. The server's guarantee is 150 ms (D-101); the plan allows 200.
	assert.Zero(t, rep.StaleFramesDropped, "frames of the old generation written after ai.interrupted")
	require.Len(t, rep.BargeInStopMS, 1)
	t.Logf("E3 client-side barge_in_stop_ms=%v (interrupting speech sent -> last old frame received)", rep.BargeInStopMS)

	// The cut turn is INTERRUPTED and what the user heard is the start of what the LLM wrote.
	eventually(t, func() bool {
		r := turnRows(t, s.ID)
		return len(r) >= 2 && r[0].Status != nil && r[1].Status != nil
	}, "both turns persisted")
	turns := turnRows(t, s.ID)
	cut := turns[0]
	assert.Equal(t, "INTERRUPTED", str(cut.Status))
	require.NotNil(t, cut.AssistantText)
	require.NotNil(t, cut.AssistantSpokenText)
	assert.NotEmpty(t, *cut.AssistantText)
	assert.True(t, strings.HasPrefix(*cut.AssistantText, *cut.AssistantSpokenText), "spoken %q is a prefix of %q", *cut.AssistantSpokenText, *cut.AssistantText)
	assert.Less(t, len(*cut.AssistantSpokenText), len(*cut.AssistantText), "the AI was cut before it finished")
	// The interruption was answered like any utterance.
	assert.Equal(t, "COMPLETED", str(turns[1].Status))
	assert.NotEmpty(t, str(turns[1].UserText))
	assert.Positive(t, rec.frameCount(*rec.of("ai.generation_started")[len(rec.of("ai.generation_started"))-1].Generation))
}
