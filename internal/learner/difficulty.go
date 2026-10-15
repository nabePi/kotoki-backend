// Package learner holds the learner-side contracts the realtime runtime needs: the adaptive
// difficulty object stored in sessions.adaptive_difficulty (spec 12.8.2) and the speed tables
// derived from it. The scoring engine itself arrives with M8.
package learner

// Skill is one of GRAMMAR, VOCABULARY, FLUENCY, NATURALNESS, LISTENING_RESPONSE,
// INTERACTION_COMPLEXITY.
type Skill string

// Formality is CASUAL, POLITE, KEIGO_LIGHT or KEIGO. KEIGO_LIGHT is an internal adaptive level
// only (R-15), never a database column value.
type Formality string

// DialectExposure is NONE, LIGHT or FULL.
type DialectExposure string

// AdaptiveDifficulty is sessions.adaptive_difficulty (JSON) and the input of the prompt builder.
// All dimensions are 1..5 unless noted; higher is more challenging.
type AdaptiveDifficulty struct {
	AlgorithmVersion       int             `json:"algorithm_version"` // = 1
	ScenarioDifficulty     int             `json:"scenario_difficulty"`
	VocabularyComplexity   int             `json:"vocabulary_complexity"`
	GrammarComplexity      int             `json:"grammar_complexity"`
	SpeechSpeed            int             `json:"speech_speed"`
	TTSSpeakingRate        float64         `json:"tts_speaking_rate"`         // derived from SpeechSpeed
	MaxAISentencesPerTurn  int             `json:"max_ai_sentences_per_turn"` // derived from SpeechSpeed
	TopicNovelty           int             `json:"topic_novelty"`
	TopicAbstraction       int             `json:"topic_abstraction"`
	QuestionComplexity     int             `json:"question_complexity"`
	FollowUpDepth          int             `json:"follow_up_depth"`
	MaxFollowUps           int             `json:"max_follow_ups"` // derived from FollowUpDepth
	ExpectedResponseLength int             `json:"expected_response_length"`
	Spontaneity            int             `json:"spontaneity"`
	Scaffolding            int             `json:"scaffolding"` // 0..3, 3 = most help
	Formality              Formality       `json:"formality"`
	DialectExposure        DialectExposure `json:"dialect_exposure"`
	TurnTakingComplexity   int             `json:"turn_taking_complexity"`
	FocusSkills            []Skill         `json:"focus_skills"`           // 0..2
	InSessionAdjustments   int             `json:"in_session_adjustments"` // 0 in the snapshot; the runtime raises it
}

var (
	ttsRates     = [5]float64{0.80, 0.88, 0.95, 1.00, 1.05}
	maxSentences = [5]int{2, 2, 3, 3, 4}
)

func speedIndex(speed int) int { return min(max(speed, 1), 5) - 1 }

// TTSSpeakingRate is the adaptiveRate factor of SpeechSpeed 1..5 (spec 12.8.3, row 3); speeds
// outside 1..5 are clamped.
func TTSSpeakingRate(speed int) float64 { return ttsRates[speedIndex(speed)] }

// MaxAISentences is the sentence cap per AI turn of SpeechSpeed 1..5 (spec 12.8.3, row 3).
func MaxAISentences(speed int) int { return maxSentences[speedIndex(speed)] }
