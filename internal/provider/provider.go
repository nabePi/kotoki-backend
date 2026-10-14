// Package provider defines the normalised ports for STT, LLM, TTS, evaluation and
// embedding vendors, the error taxonomy and (in later files) the router. It imports
// only the standard library (no net/http), uuid, x/sync/semaphore, prometheus and
// platform/clock; vendor adapters, fakes and helpers live in sub-packages that import
// this one, never the other way round (spec 8.1).
package provider

// Name identifies a vendor/adapter, UPPER_SNAKE_CASE everywhere (DB columns, JSON,
// metric labels). Vendor selection in env uses per-capability boolean flags instead.
type Name string

// Vendor names.
const (
	ElevenLabs   Name = "ELEVENLABS"
	Cartesia     Name = "CARTESIA"
	OpenAI       Name = "OPENAI"
	DeepSeek     Name = "DEEPSEEK"
	FakePrimary  Name = "FAKE_PRIMARY"  // fake for every capability (fake mode)
	FakeFallback Name = "FAKE_FALLBACK" // only the TTS fallback slot (voice.fallback) in fake mode
)

// Capability is UPPERCASE everywhere (metric labels, JSON, fault-injection keys).
type Capability string

// Capabilities.
const (
	CapSTT        Capability = "STT"
	CapLLM        Capability = "LLM"
	CapTTS        Capability = "TTS"
	CapEvaluation Capability = "EVALUATION"
	CapEmbedding  Capability = "EMBEDDING"
)

// Role is a chat message role.
type Role string

// Message roles.
const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Message is one chat message.
type Message struct {
	Role    Role
	Content string
}

// TokenUsage is LLM token usage. Estimated is true when the vendor sent no usage
// (stream cancelled or cut) and the value comes from the estimator (8.7.3).
type TokenUsage struct {
	InputTokens       int
	OutputTokens      int
	CachedInputTokens int // part of InputTokens served from the vendor cache; 0 when absent
	Estimated         bool
}

// Audio formats (D-072, D-074).
const (
	STTSampleRate = 16000 // PCM16LE mono, input to STT
	TTSSampleRate = 24000 // PCM16LE mono, output of TTS
)
