package kotokiclient

import "github.com/nabePi/kotoki-backend/internal/conversation/protocol"

// ServerLatency is the latency the server reports on a finished turn (turn.completed.latency),
// printed beside the client-side measurements.
type ServerLatency struct {
	TurnSeq int `json:"turn_seq"`
	protocol.Latency
}

// Report is what one scripted conversation measured on the client side (§14.11.1). Durations
// are milliseconds; the slices hold one entry per occurrence, in order. The JSON names are those
// of the CLI's summary.json.
type Report struct {
	ConnectMS       int64           `json:"connect_ms"`
	StartMS         int64           `json:"start_ms"`
	TurnLatencyMS   []int64         `json:"turn_latency_ms"`
	BargeInStopMS   []int64         `json:"barge_in_stop_ms"`
	ResumeMS        []int64         `json:"resume_ms"`
	ServerLatencies []ServerLatency `json:"server_latencies"`
	ChargedSeconds  int             `json:"charged_seconds"`
	Status          string          `json:"status"`
}
