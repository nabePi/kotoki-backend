package config

import (
	"os"
	"time"

	"github.com/caarlos0/env/v11"
)

// RealtimeTuning holds the conversation-runtime knobs of spec 7.2 that are not listener or
// capacity settings (those live on Realtime). PROVIDER_SESSION_GRACE and
// PROVIDER_TIMEOUT_STT_FINAL belong to provider.RouterConfig and are deliberately absent.
type RealtimeTuning struct {
	PodID string `env:"REALTIME_POD_ID"`

	WSHandshakeTimeout  time.Duration `env:"WS_HANDSHAKE_TIMEOUT" envDefault:"10s"`
	SessionStartTimeout time.Duration `env:"SESSION_START_TIMEOUT" envDefault:"10s"`
	WSPingInterval      time.Duration `env:"WS_PING_INTERVAL" envDefault:"15s"`
	WSIdleTimeout       time.Duration `env:"WS_IDLE_TIMEOUT" envDefault:"45s"`
	WSMaxJSONBytes      int64         `env:"WS_MAX_JSON_BYTES" envDefault:"16384"`
	WSMaxBinaryBytes    int64         `env:"WS_MAX_BINARY_BYTES" envDefault:"4096"`
	WSBinaryRate        int           `env:"WS_BINARY_RATE" envDefault:"100"`
	WSBinaryBurst       int           `env:"WS_BINARY_BURST" envDefault:"200"`
	WSJSONRate          int           `env:"WS_JSON_RATE" envDefault:"20"`
	WSJSONBurst         int           `env:"WS_JSON_BURST" envDefault:"40"`
	WSOutboundQueue     int           `env:"WS_OUTBOUND_QUEUE" envDefault:"256"`
	WSWriteTimeout      time.Duration `env:"WS_WRITE_TIMEOUT" envDefault:"5s"`

	VADMode                int `env:"VAD_MODE" envDefault:"2"`
	VADSpeechStartMS       int `env:"VAD_SPEECH_START_MS" envDefault:"200"`
	VADSpeechStartWindowMS int `env:"VAD_SPEECH_START_WINDOW_MS" envDefault:"300"`
	VADSpeechEndMS         int `env:"VAD_SPEECH_END_MS" envDefault:"300"`

	TurnSilenceDefaultMS    int      `env:"TURN_SILENCE_DEFAULT_MS" envDefault:"800"`
	TurnSilenceFillerMS     int      `env:"TURN_SILENCE_FILLER_MS" envDefault:"1400"`
	TurnSilenceCompleteMS   int      `env:"TURN_SILENCE_COMPLETE_MS" envDefault:"500"`
	TurnSilenceFillerOnlyMS int      `env:"TURN_SILENCE_FILLER_ONLY_MS" envDefault:"2000"`
	TurnMaxUtteranceMS      int      `env:"TURN_MAX_UTTERANCE_MS" envDefault:"45000"`
	TurnFillerSuffixes      []string `env:"TURN_FILLER_SUFFIXES" envSeparator:"," envDefault:"えっと,ええと,あの,その,なんか,まあ,て,で,けど,が,から,し,とか,って"`
	TurnCompletionSuffixes  []string `env:"TURN_COMPLETION_SUFFIXES" envSeparator:"," envDefault:"。,？,?,か,ね,よ,です,ます,ました,でした,ません,ください"`

	BargeinVADMS           int `env:"BARGEIN_VAD_MS" envDefault:"300"`
	BargeinWindowMS        int `env:"BARGEIN_WINDOW_MS" envDefault:"400"`
	BargeinMinPartialChars int `env:"BARGEIN_MIN_PARTIAL_CHARS" envDefault:"2"`

	AudioMaxLeadMS        int `env:"AUDIO_MAX_LEAD_MS" envDefault:"400"`
	PlaybackFinishGraceMS int `env:"PLAYBACK_FINISH_GRACE_MS" envDefault:"1500"`
	OpusBitrate           int `env:"OPUS_BITRATE" envDefault:"32000"`
	OpusComplexity        int `env:"OPUS_COMPLEXITY" envDefault:"5"`

	ReconnectWindow       time.Duration   `env:"RECONNECT_WINDOW" envDefault:"30s"`
	SessionWarningOffsets []time.Duration `env:"SESSION_WARNING_OFFSETS" envSeparator:"," envDefault:"60s,10s"`
	SessionExpiryGrace    time.Duration   `env:"SESSION_EXPIRY_GRACE" envDefault:"10s"`
	BalanceTickInterval   time.Duration   `env:"BALANCE_TICK_INTERVAL" envDefault:"10s"`

	ReplayMaxLen         int           `env:"REPLAY_MAXLEN" envDefault:"500"`
	ReplayTTL            time.Duration `env:"REPLAY_TTL" envDefault:"120s"`
	OwnerTTL             time.Duration `env:"OWNER_TTL" envDefault:"15s"`
	OwnerRefreshInterval time.Duration `env:"OWNER_REFRESH_INTERVAL" envDefault:"5s"`
	HeartbeatInterval    time.Duration `env:"HEARTBEAT_INTERVAL" envDefault:"5s"`
	HeartbeatTTL         time.Duration `env:"HEARTBEAT_TTL" envDefault:"60s"`

	ContextRecentTurns         int           `env:"CONTEXT_RECENT_TURNS" envDefault:"8"`
	ContextRecentTurnsMax      int           `env:"CONTEXT_RECENT_TURNS_MAX" envDefault:"12"`
	RollingSummaryTriggerTurns int           `env:"ROLLING_SUMMARY_TRIGGER_TURNS" envDefault:"4"`
	RollingSummaryTimeout      time.Duration `env:"ROLLING_SUMMARY_TIMEOUT" envDefault:"10s"`
	// RollingSummaryLLMModel empty means the active vendor's LLM model (spec 7.8.5).
	RollingSummaryLLMModel    string `env:"ROLLING_SUMMARY_LLM_MODEL"`
	MaxConsecutiveFailedTurns int    `env:"MAX_CONSECUTIVE_FAILED_TURNS" envDefault:"3"`

	DrainSafepointTimeout time.Duration `env:"DRAIN_SAFEPOINT_TIMEOUT" envDefault:"20s"`
	DrainFlushTimeout     time.Duration `env:"DRAIN_FLUSH_TIMEOUT" envDefault:"5s"`
}

// defaultRealtimeTuning returns the spec 7.2 defaults, taken from the envDefault tags so the
// table lives in one place; PodID falls back to the hostname.
func defaultRealtimeTuning() RealtimeTuning {
	var c RealtimeTuning
	if err := env.ParseWithOptions(&c, env.Options{Environment: map[string]string{}}); err != nil {
		panic("config: invalid RealtimeTuning envDefault: " + err.Error())
	}
	c.PodID, _ = os.Hostname()
	return c
}

// Validate rejects non-positive values and an inverted silence ladder (complete must be shorter
// than the default window, otherwise a finished sentence would wait longer than an open one).
func (c RealtimeTuning) Validate() error {
	v := &validator{}
	for _, d := range []struct {
		name string
		val  time.Duration
	}{
		{"WS_HANDSHAKE_TIMEOUT", c.WSHandshakeTimeout}, {"SESSION_START_TIMEOUT", c.SessionStartTimeout},
		{"WS_PING_INTERVAL", c.WSPingInterval}, {"WS_IDLE_TIMEOUT", c.WSIdleTimeout}, {"WS_WRITE_TIMEOUT", c.WSWriteTimeout},
		{"RECONNECT_WINDOW", c.ReconnectWindow}, {"SESSION_EXPIRY_GRACE", c.SessionExpiryGrace},
		{"BALANCE_TICK_INTERVAL", c.BalanceTickInterval}, {"REPLAY_TTL", c.ReplayTTL}, {"OWNER_TTL", c.OwnerTTL},
		{"OWNER_REFRESH_INTERVAL", c.OwnerRefreshInterval}, {"HEARTBEAT_INTERVAL", c.HeartbeatInterval},
		{"HEARTBEAT_TTL", c.HeartbeatTTL}, {"ROLLING_SUMMARY_TIMEOUT", c.RollingSummaryTimeout},
		{"DRAIN_SAFEPOINT_TIMEOUT", c.DrainSafepointTimeout}, {"DRAIN_FLUSH_TIMEOUT", c.DrainFlushTimeout},
	} {
		v.positive(d.name, d.val)
	}
	for _, n := range []struct {
		name string
		val  int64
	}{
		{"WS_MAX_JSON_BYTES", c.WSMaxJSONBytes}, {"WS_MAX_BINARY_BYTES", c.WSMaxBinaryBytes},
		{"WS_BINARY_RATE", int64(c.WSBinaryRate)}, {"WS_BINARY_BURST", int64(c.WSBinaryBurst)},
		{"WS_JSON_RATE", int64(c.WSJSONRate)}, {"WS_JSON_BURST", int64(c.WSJSONBurst)},
		{"WS_OUTBOUND_QUEUE", int64(c.WSOutboundQueue)}, {"VAD_SPEECH_START_MS", int64(c.VADSpeechStartMS)},
		{"VAD_SPEECH_START_WINDOW_MS", int64(c.VADSpeechStartWindowMS)}, {"VAD_SPEECH_END_MS", int64(c.VADSpeechEndMS)},
		{"TURN_SILENCE_DEFAULT_MS", int64(c.TurnSilenceDefaultMS)}, {"TURN_SILENCE_FILLER_MS", int64(c.TurnSilenceFillerMS)},
		{"TURN_SILENCE_COMPLETE_MS", int64(c.TurnSilenceCompleteMS)}, {"TURN_SILENCE_FILLER_ONLY_MS", int64(c.TurnSilenceFillerOnlyMS)},
		{"TURN_MAX_UTTERANCE_MS", int64(c.TurnMaxUtteranceMS)}, {"BARGEIN_VAD_MS", int64(c.BargeinVADMS)},
		{"BARGEIN_WINDOW_MS", int64(c.BargeinWindowMS)}, {"BARGEIN_MIN_PARTIAL_CHARS", int64(c.BargeinMinPartialChars)},
		{"AUDIO_MAX_LEAD_MS", int64(c.AudioMaxLeadMS)}, {"PLAYBACK_FINISH_GRACE_MS", int64(c.PlaybackFinishGraceMS)},
		{"OPUS_BITRATE", int64(c.OpusBitrate)}, {"OPUS_COMPLEXITY", int64(c.OpusComplexity)},
		{"REPLAY_MAXLEN", int64(c.ReplayMaxLen)}, {"CONTEXT_RECENT_TURNS", int64(c.ContextRecentTurns)},
		{"CONTEXT_RECENT_TURNS_MAX", int64(c.ContextRecentTurnsMax)},
		{"ROLLING_SUMMARY_TRIGGER_TURNS", int64(c.RollingSummaryTriggerTurns)},
		{"MAX_CONSECUTIVE_FAILED_TURNS", int64(c.MaxConsecutiveFailedTurns)},
	} {
		if n.val <= 0 {
			v.fail(n.name, "must be > 0, got %d", n.val)
		}
	}
	if c.TurnSilenceCompleteMS >= c.TurnSilenceDefaultMS {
		v.fail("TURN_SILENCE_COMPLETE_MS", "%d must be < TURN_SILENCE_DEFAULT_MS %d", c.TurnSilenceCompleteMS, c.TurnSilenceDefaultMS)
	}
	return v.err()
}
