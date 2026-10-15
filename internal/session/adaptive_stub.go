package session

import (
	"context"
	"encoding/json"
)

// NeutralAdaptiveProvider is the explicit stand-in for the learner-side adaptive difficulty (M8): it
// answers the same neutral snapshot for everyone, except formality, which is the base formality of
// the scenario version (there is no learner preference yet). M8 replaces it with an adapter over
// learner.Service behind the same AdaptiveProvider port.
type NeutralAdaptiveProvider struct{}

var _ AdaptiveProvider = NeutralAdaptiveProvider{}

// neutralAdaptive is sessions.adaptive_difficulty (spec 12.8.2) with every dial in the middle.
type neutralAdaptive struct {
	AlgorithmVersion       int      `json:"algorithm_version"`
	ScenarioDifficulty     int      `json:"scenario_difficulty"`
	VocabularyComplexity   int      `json:"vocabulary_complexity"`
	GrammarComplexity      int      `json:"grammar_complexity"`
	SpeechSpeed            int      `json:"speech_speed"`
	TTSSpeakingRate        float64  `json:"tts_speaking_rate"`
	MaxAISentencesPerTurn  int      `json:"max_ai_sentences_per_turn"`
	TopicNovelty           int      `json:"topic_novelty"`
	TopicAbstraction       int      `json:"topic_abstraction"`
	QuestionComplexity     int      `json:"question_complexity"`
	FollowUpDepth          int      `json:"follow_up_depth"`
	MaxFollowUps           int      `json:"max_follow_ups"`
	ExpectedResponseLength int      `json:"expected_response_length"`
	Spontaneity            int      `json:"spontaneity"`
	Scaffolding            int      `json:"scaffolding"`
	Formality              string   `json:"formality"`
	DialectExposure        string   `json:"dialect_exposure"`
	TurnTakingComplexity   int      `json:"turn_taking_complexity"`
	FocusSkills            []string `json:"focus_skills"`
	InSessionAdjustments   int      `json:"in_session_adjustments"`
}

// Adaptive implements AdaptiveProvider.
func (NeutralAdaptiveProvider) Adaptive(_ context.Context, req AdaptiveRequest) (json.RawMessage, error) {
	return json.Marshal(neutralAdaptive{
		AlgorithmVersion: 1, ScenarioDifficulty: 3, VocabularyComplexity: 3, GrammarComplexity: 3, SpeechSpeed: 3, TTSSpeakingRate: 0.95,
		MaxAISentencesPerTurn: 3, TopicNovelty: 3, TopicAbstraction: 3, QuestionComplexity: 3, FollowUpDepth: 3, MaxFollowUps: 2,
		ExpectedResponseLength: 3, Spontaneity: 3, Scaffolding: 1, Formality: string(req.Config.ConfigSnapshot.Formality),
		DialectExposure: "NONE", TurnTakingComplexity: 3, FocusSkills: []string{}, InSessionAdjustments: 0,
	})
}
