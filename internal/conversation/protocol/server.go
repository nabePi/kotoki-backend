package protocol

import "github.com/google/uuid"

// AudioStream adalah parameter audio + header frame biner (session.ready).
type AudioStream struct {
	AudioParams
	FrameHeader string `json:"frame_header"`
}

// ReadyAudio memuat arah upstream/downstream.
type ReadyAudio struct {
	Upstream   AudioStream `json:"upstream"`
	Downstream AudioStream `json:"downstream"`
}

// Limits adalah batas koneksi (§7.4.10).
type Limits struct {
	MaxJSONBytes       int `json:"max_json_bytes"`
	MaxBinaryBytes     int `json:"max_binary_bytes"`
	MaxBinaryPerSecond int `json:"max_binary_per_second"`
}

// SessionReady adalah payload session.ready.
type SessionReady struct {
	Status                string     `json:"status"`
	ExpectedCommand       string     `json:"expected_command"`
	ConversationStartMode string     `json:"conversation_start_mode"`
	ProtocolVersion       int        `json:"protocol_version"`
	Audio                 ReadyAudio `json:"audio"`
	Limits                Limits     `json:"limits"`
	HeartbeatIntervalS    int        `json:"heartbeat_interval_s"`
	ServerTime            Timestamp  `json:"server_time"`
}

// SessionStarted adalah payload session.started.
type SessionStarted struct {
	Status                string    `json:"status"`
	ConversationStartMode string    `json:"conversation_start_mode"`
	StartedAt             Timestamp `json:"started_at"`
	ExpiresAt             Timestamp `json:"expires_at"`
	DurationSeconds       int       `json:"duration_seconds"`
	RemainingSeconds      int       `json:"remaining_seconds"`
	TurnState             string    `json:"turn_state"`
}

// SnapshotEnding adalah objek `ending` pada snapshot.
type SnapshotEnding struct {
	Stage        string `json:"stage"`
	Reason       string `json:"reason"`
	GraceSeconds int    `json:"grace_seconds"`
}

// SnapshotSession adalah objek `session` pada snapshot.
type SnapshotSession struct {
	Status                string          `json:"status"`
	ConversationStartMode string          `json:"conversation_start_mode"`
	StartedAt             Timestamp       `json:"started_at"`
	ExpiresAt             Timestamp       `json:"expires_at"`
	RemainingSeconds      int             `json:"remaining_seconds"`
	Ending                *SnapshotEnding `json:"ending"`
}

// RecentTurn adalah ringkasan turn final pada snapshot.
type RecentTurn struct {
	TurnID              uuid.UUID `json:"turn_id"`
	TurnSeq             int       `json:"turn_seq"`
	Status              string    `json:"status"`
	UserText            *string   `json:"user_text"`
	AssistantSpokenText *string   `json:"assistant_spoken_text"`
}

// SessionSnapshot adalah payload session.snapshot.
type SessionSnapshot struct {
	Replay            string          `json:"replay"`
	ReplayedCount     int             `json:"replayed_count"`
	ServerSeqBaseline int64           `json:"server_seq_baseline"`
	Session           SnapshotSession `json:"session"`
	TurnState         string          `json:"turn_state"`
	ActiveGeneration  uint32          `json:"active_generation"`
	NextTurnSeq       int             `json:"next_turn_seq"`
	RecentTurns       []RecentTurn    `json:"recent_turns"`
}

// SessionEnding adalah payload session.ending.
type SessionEnding struct {
	Stage            string `json:"stage"`
	Reason           string `json:"reason"`
	RemainingSeconds int    `json:"remaining_seconds"`
	GraceSeconds     int    `json:"grace_seconds"`
}

// SessionCompleted adalah payload session.completed.
type SessionCompleted struct {
	EndReason           string    `json:"end_reason"`
	StartedAt           Timestamp `json:"started_at"`
	EndedAt             Timestamp `json:"ended_at"`
	ChargedSeconds      int       `json:"charged_seconds"`
	DisconnectedSeconds int       `json:"disconnected_seconds"`
	TurnCount           int       `json:"turn_count"`
}

// SessionFailed adalah payload session.failed.
type SessionFailed struct {
	EndReason      string `json:"end_reason"`
	ErrorCode      string `json:"error_code"`
	ChargedSeconds int    `json:"charged_seconds"`
}

// SpeechStarted adalah payload speech.started.
type SpeechStarted struct {
	AudioOffsetMS  int64 `json:"audio_offset_ms"`
	DuringAISpeech bool  `json:"during_ai_speech"`
}

// SpeechEnded adalah payload speech.ended.
type SpeechEnded struct {
	AudioOffsetMS int64 `json:"audio_offset_ms"`
	SpeechMS      int   `json:"speech_ms"`
}

// TurnStarted adalah payload turn.started.
type TurnStarted struct {
	Initiator string    `json:"initiator"`
	StartedAt Timestamp `json:"started_at"`
}

// STTPartial adalah payload stt.partial.
type STTPartial struct {
	Text string `json:"text"`
}

// STTFinal adalah payload stt.final.
type STTFinal struct {
	Text         string `json:"text"`
	EndReason    string `json:"end_reason"`
	STTFinalMS   int    `json:"stt_final_ms"`
	Provider     string `json:"provider"`
	UsedFallback bool   `json:"used_fallback"`
}

// AIGenerationStarted adalah payload ai.generation_started.
type AIGenerationStarted struct {
	Kind string `json:"kind"`
}

// AITextDelta adalah payload ai.text_delta.
type AITextDelta struct {
	Text string `json:"text"`
}

// AISentence adalah payload ai.sentence (tanpa expression, D-014).
type AISentence struct {
	SentenceIndex int    `json:"sentence_index"`
	Text          string `json:"text"`
	FirstFrameSeq uint32 `json:"first_frame_seq"`
	StartMS       int    `json:"start_ms"`
}

// AIAudioStarted adalah payload ai.audio_started.
type AIAudioStarted struct {
	AudioParams
}

// AIAudioCompleted adalah payload ai.audio_completed.
type AIAudioCompleted struct {
	FrameCount    int  `json:"frame_count"`
	TotalAudioMS  int  `json:"total_audio_ms"`
	SentenceCount int  `json:"sentence_count"`
	LLMFinished   bool `json:"llm_finished"`
}

// AIInterrupted adalah payload ai.interrupted.
type AIInterrupted struct {
	Reason              string `json:"reason"`
	ActiveGeneration    uint32 `json:"active_generation"`
	SpokenSentenceCount int    `json:"spoken_sentence_count"`
}

// Latency adalah metrik latensi turn (opsional).
type Latency struct {
	TurnEndToFirstAudioMS int `json:"turn_end_to_first_audio_ms"`
	STTFinalMS            int `json:"stt_final_ms"`
	LLMFirstTokenMS       int `json:"llm_first_token_ms"`
	TTSFirstAudioMS       int `json:"tts_first_audio_ms"`
}

// TurnCompleted adalah payload turn.completed.
type TurnCompleted struct {
	Status              string   `json:"status"`
	UserText            *string  `json:"user_text"`
	AssistantText       *string  `json:"assistant_text"`
	AssistantSpokenText *string  `json:"assistant_spoken_text"`
	Latency             *Latency `json:"latency,omitempty"`
}

// BalanceTick adalah payload balance.tick.
type BalanceTick struct {
	RemainingSeconds int       `json:"remaining_seconds"`
	ElapsedSeconds   int       `json:"elapsed_seconds"`
	ExpiresAt        Timestamp `json:"expires_at"`
}

// Pong adalah payload pong.
type Pong struct {
	ClientTS int64 `json:"client_ts"`
	ServerTS int64 `json:"server_ts"`
}

// ErrorBody memakai format tetap D-013 (keempat field selalu ada).
type ErrorBody struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
	Details   any    `json:"details"`
}

// ErrorPayload adalah payload error.
type ErrorPayload struct {
	Error      ErrorBody `json:"error"`
	Retryable  bool      `json:"retryable"`
	Fatal      bool      `json:"fatal"`
	RefEventID *string   `json:"ref_event_id"`
}
