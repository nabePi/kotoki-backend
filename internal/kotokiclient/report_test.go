package kotokiclient

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/conversation/protocol"
)

func TestReportJSONShape(t *testing.T) {
	b, err := json.Marshal(Report{
		ConnectMS: 12, StartMS: 34, TurnLatencyMS: []int64{900}, BargeInStopMS: []int64{120}, ResumeMS: []int64{200},
		ServerLatencies: []ServerLatency{{TurnSeq: 1, Latency: protocol.Latency{TurnEndToFirstAudioMS: 800}}},
		ChargedSeconds:  42, Status: "COMPLETED",
	})
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(b, &m))
	for _, k := range []string{"connect_ms", "start_ms", "turn_latency_ms", "barge_in_stop_ms", "resume_ms", "server_latencies", "charged_seconds", "status"} {
		require.Contains(t, m, k)
	}
	sl := m["server_latencies"].([]any)[0].(map[string]any)
	require.EqualValues(t, 1, sl["turn_seq"])
	require.EqualValues(t, 800, sl["turn_end_to_first_audio_ms"])
}
