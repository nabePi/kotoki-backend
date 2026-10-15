package provider

import (
	"context"
	"time"
)

// TTSRequest is ONE sentence (D-081); output is PCM16LE mono 24 kHz. No expression
// or style (D-014): only voice, model, tempo and text.
type TTSRequest struct {
	Text     string // one sentence, not empty, ≤ 300 runes
	Language string // "ja"
	// Voice comes from sessions.config_snapshot.character and is read by the router, not the adapter.
	Voice VoiceSelection
	// SpeakingRate = clamp(voice.speaking_rate × adaptiveRate, 0.80, 1.05), computed by the
	// runtime; 0 → 1.0. The router rejects values outside 0.80–1.05 as INVALID_REQUEST.
	SpeakingRate float64
	PreviousText string // previous sentence of the same generation; may be empty
	RequestID    string // "{session_id}:{generation}:{sentence_index}"

	// Filled by the router once the sentence voice is chosen (8.4.12); adapters read only
	// VoiceID, Model, SpeakingRate, Text, PreviousText and Language.
	VoiceID string
	Model   string
}

// VoiceSelection is the per-character voice pair used for every sentence of a session
// (D-015): the vendor never comes from env.
type VoiceSelection struct {
	Primary  VoiceRef
	Fallback *VoiceRef // nil: no TTS fallback (failure → generation FAILED → apology, D-042)
}

// VoiceRef names a vendor voice.
type VoiceRef struct {
	Provider  Name   // ELEVENLABS | CARTESIA; Fallback.Provider differs from Primary.Provider
	ModelName string // free vendor text, e.g. "sonic3.6"
	VoiceID   string
}

// TTSProvider synthesises one sentence.
type TTSProvider interface {
	Name() Name
	// Synthesize returns after the vendor accepted the request; mid-stream errors arrive
	// as the last chunk's Err. Cancelling ctx closes the connection within 50 ms.
	Synthesize(ctx context.Context, req TTSRequest) (TTSStream, error)
}

// TTSStream delivers chunks; buffered (32) and closed after the Final chunk or Err.
type TTSStream interface {
	Chunks() <-chan TTSChunk
	Close() error
}

// TTSChunk is a slice of audio.
type TTSChunk struct {
	PCM        []byte // PCM16LE mono 24 kHz, even length
	Final      bool
	Usage      *TTSUsage // Final chunk only
	Provider   Name
	Model      string
	Epoch      int // filled by the router (8.5.7); adapters leave 0
	ReceivedAt time.Time
	Err        *Error
}

// TTSUsage is metered per request.
type TTSUsage struct {
	Characters int   // utf8.RuneCountInString(req.Text), computed by the adapter
	AudioMs    int64 // duration of the PCM produced
}
