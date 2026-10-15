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
	// StaleFramesDropped counts downstream frames of an old generation that reached the client
	// after the event that replaced it and were discarded (Review Focus 2): zero is the
	// server's guarantee that nothing of an interrupted answer is written after ai.interrupted.
	StaleFramesDropped int `json:"stale_frames_dropped"`
	// CloseCode is the WebSocket close code (§7.4.8) the last connection ended with, 0 when it
	// ended without a close frame (a Drop, a lost transport).
	CloseCode      int    `json:"close_code"`
	ChargedSeconds int    `json:"charged_seconds"`
	Status         string `json:"status"`
}
