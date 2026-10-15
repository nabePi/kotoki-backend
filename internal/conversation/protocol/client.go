package protocol

// AudioParams adalah parameter audio yang diminta client (§7.4.4); hanya satu
// kombinasi yang valid.
type AudioParams struct {
	Codec      string `json:"codec"`
	SampleRate int    `json:"sample_rate"`
	Channels   int    `json:"channels"`
	FrameMS    int    `json:"frame_ms"`
}

// Valid benar bila parameter persis opus/48000/1/20.
func (a AudioParams) Valid() bool {
	return a.Codec == "opus" && a.SampleRate == 48000 && a.Channels == 1 && a.FrameMS == 20
}

// ClientInfo adalah info client opsional pada session.start.
type ClientInfo struct {
	AppVersion string `json:"app_version"`
	Platform   string `json:"platform"`
}

// SessionStart adalah payload session.start.
type SessionStart struct {
	Client *ClientInfo `json:"client,omitempty"`
	Audio  AudioParams `json:"audio"`
}

// SessionResume adalah payload session.resume.
type SessionResume struct {
	LastServerSeq int64       `json:"last_server_seq"`
	Audio         AudioParams `json:"audio"`
}

// SessionEnd adalah payload session.end.
type SessionEnd struct {
	Reason string `json:"reason"`
}

// ClientBargeIn adalah payload client.barge_in (hint saja, D-077).
type ClientBargeIn struct {
	Generation uint32 `json:"generation"`
	VadMS      *int   `json:"vad_ms,omitempty"`
}

// ClientPlaybackFinished adalah payload client.playback_finished.
type ClientPlaybackFinished struct {
	Generation uint32 `json:"generation"`
	PlayedMS   int    `json:"played_ms"`
}

// Ping adalah payload ping.
type Ping struct {
	ClientTS int64 `json:"client_ts"`
}
