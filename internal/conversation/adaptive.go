package conversation

import (
	"regexp"

	"github.com/nabePi/kotoki-backend/internal/learner"
)

// ClarificationRegex marks a user text that asks for repetition, slower speech or help (spec
// 12.8.5).
var ClarificationRegex = regexp.MustCompile(`もう一度|もう一回|ゆっくり|わかりません|分かりません|わからない|分からない|聞き取れ|何て言いました|なんて言いました|どういう意味`)

const (
	adaptiveSignalWindow   = 6 // user turns inspected
	adaptiveMinSignals     = 2
	adaptiveMaxAdjustments = 2
	adaptiveMinTurnsGap    = 4
	maxScaffolding         = 3
)

// AdaptiveState is the in-session adaptive state of the runtime (spec 7.8.6).
type AdaptiveState struct {
	Current          learner.AdaptiveDifficulty // starts as the sessions.adaptive_difficulty snapshot
	TurnsSinceAdjust int                        // starts at 0; +1 per COMPLETED turn
}

// MaybeAdjust is the pure in-session adjustment of spec 12.8.5 / 7.8.6. It counts one COMPLETED
// turn, then lowers speech speed and question complexity and raises scaffolding when at least two
// of the last six user texts ask for clarification. It returns the new state and whether an
// adjustment happened; the input state and slice are not modified.
func MaybeAdjust(s AdaptiveState, recentUserTexts []string) (AdaptiveState, bool) {
	s.TurnsSinceAdjust++
	if len(recentUserTexts) > adaptiveSignalWindow {
		recentUserTexts = recentUserTexts[len(recentUserTexts)-adaptiveSignalWindow:]
	}
	signals := 0
	for _, t := range recentUserTexts {
		if ClarificationRegex.MatchString(t) {
			signals++
		}
	}
	if signals < adaptiveMinSignals || s.Current.InSessionAdjustments >= adaptiveMaxAdjustments ||
		s.TurnsSinceAdjust < adaptiveMinTurnsGap {
		return s, false
	}
	c := s.Current // FocusSkills is shared but never written here
	c.SpeechSpeed = max(1, c.SpeechSpeed-1)
	c.TTSSpeakingRate = learner.TTSSpeakingRate(c.SpeechSpeed)
	c.MaxAISentencesPerTurn = learner.MaxAISentences(c.SpeechSpeed)
	c.QuestionComplexity = max(1, c.QuestionComplexity-1)
	c.Scaffolding = min(maxScaffolding, c.Scaffolding+1)
	c.InSessionAdjustments++
	s.Current = c
	s.TurnsSinceAdjust = 0
	return s, true
}
