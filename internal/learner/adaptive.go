package learner

import "math"

// Formality values. KEIGO_LIGHT is an internal adaptive level only (R-15).
const (
	FormalityCasual     Formality = "CASUAL"
	FormalityPolite     Formality = "POLITE"
	FormalityKeigoLight Formality = "KEIGO_LIGHT"
	FormalityKeigo      Formality = "KEIGO"
)

// DialectExposure values.
const (
	DialectNone  DialectExposure = "NONE"
	DialectLight DialectExposure = "LIGHT"
	DialectFull  DialectExposure = "FULL"
)

// Adaptive difficulty constants (spec 12.8.3).
const (
	stretchMargin        = 0.3
	keigoLightBelow      = 2.5 // L(GRAMMAR) below this softens KEIGO to KEIGO_LIGHT
	dialectOffBelow      = 2.0
	dialectFullAtLeast   = 3.5
	dialectFullSessions  = 2
	scaffoldHighGap      = 1.5
	scaffoldMidGap       = 0.75
	topicNoveltyStep     = 0.5
	topicNoveltyMaxCount = 4

	// adaptiveEpsilon keeps levels stored with 2 decimals from straddling a threshold or a .5
	// rounding boundary through float noise (e.g. 2.2+0.3).
	adaptiveEpsilon = 1e-9
)

// roundHalfUp is R(x) = floor(x + 0.5).
func roundHalfUp(x float64) int { return int(math.Floor(x + 0.5 + adaptiveEpsilon)) }

// Stretch is clamp(R(x+0.3), max(1, b-2), min(5, b+1)): slightly above the learner's level but
// bounded so the scenario keeps its character.
func Stretch(x float64, b int) int {
	lo, hi := max(1, b-2), min(5, b+1)
	return min(max(roundHalfUp(x+stretchMargin), lo), hi)
}

// MaxFollowUps derives the follow-up cap from FollowUpDepth: 1->0, 2->1, 3->2, 4->3, 5->3.
func MaxFollowUps(depth int) int { return min(max(depth-1, 0), 3) }

// ComputeAdaptiveDifficulty is the pure function of spec 12.8.3 (13 dimensions plus the
// derived tempo fields). The final speaking rate (voice rate x TTSSpeakingRate) is a runtime
// concern, not computed here.
func ComputeAdaptiveDifficulty(in AdaptiveInput) AdaptiveDifficulty {
	b := in.ScenarioDifficulty
	l := func(k Skill) float64 { return in.Levels[k] }
	mean := func(a, c Skill) float64 { return (l(a) + l(c)) / 2 }

	speed := Stretch(l(SkillListeningResponse), b)
	switch in.PrefSpeechSpeed {
	case "SLOWER":
		speed--
	case "FASTER":
		speed++
	}
	speed = min(max(speed, 1), 5)

	followUp := Stretch(l(SkillInteractionComplexity), b)

	return AdaptiveDifficulty{
		AlgorithmVersion:       AlgorithmVersion,
		ScenarioDifficulty:     b,
		VocabularyComplexity:   Stretch(l(SkillVocabulary), b),
		GrammarComplexity:      Stretch(l(SkillGrammar), b),
		SpeechSpeed:            speed,
		TTSSpeakingRate:        TTSSpeakingRate(speed),
		MaxAISentencesPerTurn:  MaxAISentences(speed),
		TopicNovelty:           topicNovelty(b, in.TopicSessions90d),
		TopicAbstraction:       Stretch(mean(SkillVocabulary, SkillInteractionComplexity), b),
		QuestionComplexity:     Stretch(l(SkillListeningResponse), b),
		FollowUpDepth:          followUp,
		MaxFollowUps:           MaxFollowUps(followUp),
		ExpectedResponseLength: Stretch(l(SkillFluency), b),
		Spontaneity:            Stretch(mean(SkillFluency, SkillInteractionComplexity), b),
		Scaffolding:            scaffolding(in),
		Formality:              formality(in),
		DialectExposure:        dialectExposure(in),
		TurnTakingComplexity:   Stretch(mean(SkillInteractionComplexity, SkillListeningResponse), b),
		FocusSkills:            in.Focus,
		InSessionAdjustments:   0,
	}
}

func topicNovelty(b, sessions int) int {
	n := float64(min(max(sessions, 0), topicNoveltyMaxCount))
	return min(max(roundHalfUp(float64(b)-1+topicNoveltyStep*n), 1), 5)
}

func scaffolding(in AdaptiveInput) int {
	lmin := math.Min(in.Levels[SkillGrammar], math.Min(in.Levels[SkillVocabulary], in.Levels[SkillListeningResponse]))
	gap := float64(in.ScenarioDifficulty) - lmin
	switch {
	case gap >= scaffoldHighGap-adaptiveEpsilon:
		return 3
	case gap >= scaffoldMidGap-adaptiveEpsilon:
		return 2
	case gap >= -adaptiveEpsilon:
		return 1
	default:
		return 0
	}
}

func formality(in AdaptiveInput) Formality {
	if in.ScenarioFormality == FormalityKeigo {
		if in.Levels[SkillGrammar] < keigoLightBelow-adaptiveEpsilon {
			return FormalityKeigoLight
		}
		return FormalityKeigo
	}
	switch in.PrefFormality {
	case "CASUAL":
		return FormalityCasual
	case "POLITE":
		return FormalityPolite
	}
	return in.ScenarioFormality
}

func dialectExposure(in AdaptiveInput) DialectExposure {
	if in.VoiceAccent != "KANSAI" || in.DialectPreference == "STANDARD_ONLY" {
		return DialectNone
	}
	ld := (in.Levels[SkillListeningResponse] + in.Levels[SkillVocabulary]) / 2
	switch {
	case ld < dialectOffBelow-adaptiveEpsilon:
		return DialectNone
	case ld >= dialectFullAtLeast-adaptiveEpsilon && in.DialectSessions90d >= dialectFullSessions:
		return DialectFull
	default:
		return DialectLight
	}
}
