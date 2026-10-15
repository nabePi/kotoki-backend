//go:build e2e

package e2e

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/kotokiclient"
)

const (
	sloTurns = 20
	// sloTurnEndToFirstAudioP95 is D-100; sloServerOverheadP95 is the server's own share of it.
	sloTurnEndToFirstAudioP95 = 1.5
	sloServerOverheadP95      = 0.05
)

// TestSLOServerSide runs 20 turns in a row against the fake providers and checks the server-side
// latency histograms of the realtime process (D-100). Fake providers have fixed latencies, so what
// is measured is the server's own overhead; the result is recorded in the implementation log.
func TestSLOServerSide(t *testing.T) {
	u := newUser(t)
	s := startSessionFor(t, u, "USER_FIRST", 600)
	before := scrape(t, stack.rt.opsAddr)

	var steps []kotokiclient.Step
	for i := 0; i < sloTurns; i++ {
		name := "ja_question"
		if i%2 == 1 {
			name = "ja_greeting_short"
		}
		steps = append(steps, kotokiclient.Speak{Packets: packets(t, name), Label: name}, kotokiclient.WaitAIDone{})
	}
	steps = append(steps, kotokiclient.End{})
	rep, err := converse(t, u, s, steps, newRecorder())
	require.NoError(t, err)
	require.Equal(t, "COMPLETED", rep.Status)

	eventually(t, func() bool {
		n := 0
		for _, r := range turnRows(t, s.ID) {
			if r.Status != nil && *r.Status == "COMPLETED" {
				n++
			}
		}
		return n == sloTurns
	}, "%d COMPLETED turns", sloTurns)

	after := scrape(t, stack.rt.opsAddr)
	e2e, n := histogram(after, before, "kotoki_realtime_turn_end_to_first_audio_seconds", `kind="reply"`)
	require.EqualValues(t, sloTurns, n, "reply turns observed")
	ovh, m := histogram(after, before, "kotoki_realtime_server_overhead_seconds")
	require.EqualValues(t, sloTurns, m, "overhead observations")

	e50, e95 := quantile(e2e, 0.5), quantile(e2e, 0.95)
	o50, o95 := quantile(ovh, 0.5), quantile(ovh, 0.95)
	t.Logf("SLO turn_end_to_first_audio_seconds: P50=%.3fs P95=%.3fs (n=%d)", e50, e95, int(n))
	t.Logf("SLO server_overhead_seconds: P50=%.3fs P95=%.3fs (n=%d)", o50, o95, int(m))
	t.Logf("SLO server_overhead_seconds: mean=%.4fs (sum/count)", (after["kotoki_realtime_server_overhead_seconds_sum"]-before["kotoki_realtime_server_overhead_seconds_sum"])/m)
	t.Logf("SLO client turn_latency_ms: %v", rep.TurnLatencyMS)
	assert.LessOrEqual(t, e95, sloTurnEndToFirstAudioP95, fmt.Sprintf("D-100: P95 turn end to first audio <= %.1f s", sloTurnEndToFirstAudioP95))
	assert.LessOrEqual(t, o95, sloServerOverheadP95, fmt.Sprintf("P95 server overhead <= %.0f ms", sloServerOverheadP95*1000))
}
