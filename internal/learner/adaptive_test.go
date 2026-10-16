package learner_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/learner"
)

func specLevels() map[learner.Skill]float64 {
	return map[learner.Skill]float64{
		learner.SkillGrammar: 2.35, learner.SkillVocabulary: 3.10, learner.SkillFluency: 2.40,
		learner.SkillNaturalness: 2.20, learner.SkillListeningResponse: 2.90, learner.SkillInteractionComplexity: 2.00,
	}
}

func specAdaptiveInput() learner.AdaptiveInput {
	fi := specFocusInput()
	return learner.AdaptiveInput{
		Levels:                specLevels(),
		Confidences:           fi.Confidences,
		FindingStats:          fi.Findings,
		EvaluationsConsidered: 3,
		ScenarioDifficulty:    4,
		ScenarioFormality:     "KEIGO",
		VoiceAccent:           "KANTO",
		TopicSessions90d:      1,
		PrefSpeechSpeed:       "NORMAL",
		PrefFormality:         "AUTO",
		DialectPreference:     "STANDARD_ONLY",
		Focus:                 learner.FocusSkills(fi),
	}
}

func TestComputeAdaptiveDifficulty_Spec1283Example(t *testing.T) {
	got := learner.ComputeAdaptiveDifficulty(specAdaptiveInput())
	raw, err := json.Marshal(got)
	require.NoError(t, err)
	require.JSONEq(t, `{
	  "algorithm_version": 1, "scenario_difficulty": 4,
	  "vocabulary_complexity": 3, "grammar_complexity": 3,
	  "speech_speed": 3, "tts_speaking_rate": 0.95, "max_ai_sentences_per_turn": 3,
	  "topic_novelty": 4, "topic_abstraction": 3, "question_complexity": 3,
	  "follow_up_depth": 2, "max_follow_ups": 1, "expected_response_length": 3,
	  "spontaneity": 3, "scaffolding": 3, "formality": "KEIGO_LIGHT", "dialect_exposure": "NONE",
	  "turn_taking_complexity": 3, "focus_skills": ["GRAMMAR", "NATURALNESS"], "in_session_adjustments": 0
	}`, string(raw))
}

func TestAdaptive_LowGrammarOnlyAffectsGrammarScaffoldingFormality(t *testing.T) {
	base := specAdaptiveInput()
	base.ScenarioDifficulty = 3
	for k := range base.Levels {
		base.Levels[k] = 3.0
	}
	b := learner.ComputeAdaptiveDifficulty(base)
	require.Equal(t, 1, b.Scaffolding)
	require.Equal(t, learner.Formality("KEIGO"), b.Formality)

	low := specAdaptiveInput()
	low.ScenarioDifficulty = 3
	for k := range low.Levels {
		low.Levels[k] = 3.0
	}
	low.Levels[learner.SkillGrammar] = 1.8
	l := learner.ComputeAdaptiveDifficulty(low)

	require.Equal(t, 2, l.GrammarComplexity)
	require.Equal(t, 3, b.GrammarComplexity)
	require.Equal(t, 2, l.Scaffolding)
	require.Equal(t, learner.Formality("KEIGO_LIGHT"), l.Formality)

	// Every other dimension is identical.
	l.GrammarComplexity, l.Scaffolding, l.Formality = b.GrammarComplexity, b.Scaffolding, b.Formality
	require.Equal(t, b, l)
}

func TestAdaptive_StretchBounds(t *testing.T) {
	// B=1: bound [1,2]; B=5: bound [3,5].
	for _, c := range []struct {
		b, lo, hi int
	}{{1, 1, 2}, {5, 3, 5}, {3, 1, 4}} {
		for _, x := range []float64{0, 1, 2.5, 3.3, 4.9, 5} {
			s := learner.Stretch(x, c.b)
			require.GreaterOrEqual(t, s, c.lo, "x=%v b=%d", x, c.b)
			require.LessOrEqual(t, s, c.hi, "x=%v b=%d", x, c.b)
		}
	}
	require.Equal(t, 2, learner.Stretch(5, 1))
	require.Equal(t, 1, learner.Stretch(0, 1))
	require.Equal(t, 3, learner.Stretch(0, 5))
	require.Equal(t, 5, learner.Stretch(5, 5))
	require.Equal(t, 3, learner.Stretch(3.1, 4)) // R(3.4)
	require.Equal(t, 4, learner.Stretch(3.2, 4)) // R(3.5) half up
}

func TestAdaptive_PrefSpeedShiftsSpeedByOne(t *testing.T) {
	in := specAdaptiveInput() // speed 3
	for pref, want := range map[string]int{"SLOWER": 2, "NORMAL": 3, "FASTER": 4} {
		in.PrefSpeechSpeed = pref
		got := learner.ComputeAdaptiveDifficulty(in)
		require.Equal(t, want, got.SpeechSpeed, pref)
		require.InDelta(t, learner.TTSSpeakingRate(want), got.TTSSpeakingRate, 1e-9)
		require.Equal(t, learner.MaxAISentences(want), got.MaxAISentencesPerTurn)
	}
	// Clamp at 1..5; question complexity is not affected by the preference.
	in.ScenarioDifficulty = 1
	in.Levels[learner.SkillListeningResponse] = 0
	in.PrefSpeechSpeed = "SLOWER"
	require.Equal(t, 1, learner.ComputeAdaptiveDifficulty(in).SpeechSpeed)
	in.ScenarioDifficulty = 5
	in.Levels[learner.SkillListeningResponse] = 5
	in.PrefSpeechSpeed = "FASTER"
	got := learner.ComputeAdaptiveDifficulty(in)
	require.Equal(t, 5, got.SpeechSpeed)
	require.Equal(t, 5, got.QuestionComplexity)
}

func TestAdaptive_DialectRules(t *testing.T) {
	cases := []struct {
		name     string
		accent   string
		pref     string
		ld       float64 // set on both LISTENING_RESPONSE and VOCABULARY
		sessions int
		want     learner.DialectExposure
	}{
		{"kanto accent", "KANTO", "ALLOW_DIALECT", 4.0, 5, "NONE"},
		{"standard only", "KANSAI", "STANDARD_ONLY", 4.0, 5, "NONE"},
		{"low Ld", "KANSAI", "ALLOW_DIALECT", 1.9, 5, "NONE"},
		{"Ld exactly 2.0", "KANSAI", "ALLOW_DIALECT", 2.0, 0, "LIGHT"},
		{"full", "KANSAI", "ALLOW_DIALECT", 3.5, 2, "FULL"},
		{"Ld just below 3.5", "KANSAI", "ALLOW_DIALECT", 3.4, 5, "LIGHT"},
		{"one dialect session", "KANSAI", "ALLOW_DIALECT", 3.5, 1, "LIGHT"},
		{"mid Ld", "KANSAI", "ALLOW_DIALECT", 2.5, 0, "LIGHT"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := specAdaptiveInput()
			in.VoiceAccent, in.DialectPreference, in.DialectSessions90d = c.accent, c.pref, c.sessions
			in.Levels[learner.SkillListeningResponse], in.Levels[learner.SkillVocabulary] = c.ld, c.ld
			require.Equal(t, c.want, learner.ComputeAdaptiveDifficulty(in).DialectExposure)
		})
	}
	// Ld is the mean of LISTENING_RESPONSE and VOCABULARY.
	in := specAdaptiveInput()
	in.VoiceAccent, in.DialectPreference, in.DialectSessions90d = "KANSAI", "ALLOW_DIALECT", 3
	in.Levels[learner.SkillListeningResponse], in.Levels[learner.SkillVocabulary] = 4.0, 3.0
	require.Equal(t, learner.DialectExposure("FULL"), learner.ComputeAdaptiveDifficulty(in).DialectExposure)
	in.Levels[learner.SkillListeningResponse], in.Levels[learner.SkillVocabulary] = 5.0, 1.9
	require.Equal(t, learner.DialectExposure("LIGHT"), learner.ComputeAdaptiveDifficulty(in).DialectExposure) // Ld 3.45 < 3.5
}

func TestAdaptive_Formality(t *testing.T) {
	cases := []struct {
		name           string
		scenario, pref string
		grammar        float64
		want           learner.Formality
	}{
		{"keigo low grammar", "KEIGO", "AUTO", 2.4, "KEIGO_LIGHT"},
		{"keigo grammar 2.5", "KEIGO", "AUTO", 2.5, "KEIGO"},
		{"keigo ignores casual", "KEIGO", "CASUAL", 4.0, "KEIGO"},
		{"keigo light ignores polite", "KEIGO", "POLITE", 1.0, "KEIGO_LIGHT"},
		{"polite casual pref", "POLITE", "CASUAL", 3, "CASUAL"},
		{"polite auto", "POLITE", "AUTO", 3, "POLITE"},
		{"casual polite pref", "CASUAL", "POLITE", 3, "POLITE"},
		{"casual auto", "CASUAL", "AUTO", 3, "CASUAL"},
		{"polite low grammar unchanged", "POLITE", "AUTO", 0.5, "POLITE"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := specAdaptiveInput()
			in.ScenarioFormality, in.PrefFormality = learner.Formality(c.scenario), c.pref
			in.Levels[learner.SkillGrammar] = c.grammar
			require.Equal(t, c.want, learner.ComputeAdaptiveDifficulty(in).Formality)
		})
	}
}

func TestAdaptive_ScaffoldingBands(t *testing.T) {
	// B=4; Lmin = min(GRAMMAR, VOCABULARY, LISTENING_RESPONSE).
	for _, c := range []struct {
		lmin float64
		want int
	}{{2.5, 3}, {2.6, 2}, {3.25, 2}, {3.26, 1}, {4.0, 1}, {4.01, 0}, {5, 0}} {
		in := specAdaptiveInput()
		in.Levels[learner.SkillGrammar] = c.lmin
		in.Levels[learner.SkillVocabulary], in.Levels[learner.SkillListeningResponse] = 5, 5
		require.Equal(t, c.want, learner.ComputeAdaptiveDifficulty(in).Scaffolding, "lmin=%v", c.lmin)
	}
	// FLUENCY, NATURALNESS and INTERACTION_COMPLEXITY do not count toward Lmin.
	in := specAdaptiveInput()
	for _, k := range []learner.Skill{learner.SkillGrammar, learner.SkillVocabulary, learner.SkillListeningResponse} {
		in.Levels[k] = 5
	}
	in.Levels[learner.SkillFluency] = 0
	require.Equal(t, 0, learner.ComputeAdaptiveDifficulty(in).Scaffolding)
}

func TestAdaptive_TopicNovelty(t *testing.T) {
	in := specAdaptiveInput()
	in.ScenarioDifficulty = 3
	for n, want := range []int{2, 3, 3, 4, 4, 4, 4} { // R(2+0.5*min(n,4))
		in.TopicSessions90d = n
		require.Equal(t, want, learner.ComputeAdaptiveDifficulty(in).TopicNovelty, "n=%d", n)
	}
	in.ScenarioDifficulty = 1
	in.TopicSessions90d = 0
	require.Equal(t, 1, learner.ComputeAdaptiveDifficulty(in).TopicNovelty) // R(0) clamped to 1
	in.ScenarioDifficulty = 5
	in.TopicSessions90d = 9
	require.Equal(t, 5, learner.ComputeAdaptiveDifficulty(in).TopicNovelty) // R(6) clamped to 5
}

func TestAdaptive_PairedDimensions(t *testing.T) {
	// Each dimension depends only on its own skills: change one skill, check exactly which move.
	base := specAdaptiveInput()
	base.ScenarioDifficulty = 3
	for k := range base.Levels {
		base.Levels[k] = 2.0
	}
	b := learner.ComputeAdaptiveDifficulty(base)
	bump := func(k learner.Skill) learner.AdaptiveDifficulty {
		in := base
		in.Levels = map[learner.Skill]float64{}
		for s, v := range base.Levels {
			in.Levels[s] = v
		}
		in.Levels[k] = 3.0
		return learner.ComputeAdaptiveDifficulty(in)
	}
	v := bump(learner.SkillVocabulary)
	require.Greater(t, v.VocabularyComplexity, b.VocabularyComplexity)
	require.Equal(t, b.GrammarComplexity, v.GrammarComplexity)
	li := bump(learner.SkillListeningResponse)
	require.Greater(t, li.QuestionComplexity, b.QuestionComplexity)
	require.Greater(t, li.SpeechSpeed, b.SpeechSpeed)
	require.Greater(t, li.TurnTakingComplexity, b.TurnTakingComplexity)
	ic := bump(learner.SkillInteractionComplexity)
	require.Greater(t, ic.FollowUpDepth, b.FollowUpDepth)
	require.Greater(t, ic.TopicAbstraction, b.TopicAbstraction)
	require.Greater(t, ic.Spontaneity, b.Spontaneity)
	require.Greater(t, ic.TurnTakingComplexity, b.TurnTakingComplexity)
	fl := bump(learner.SkillFluency)
	require.Greater(t, fl.ExpectedResponseLength, b.ExpectedResponseLength)
	require.Equal(t, b.FollowUpDepth, fl.FollowUpDepth)
	require.Equal(t, b.TurnTakingComplexity, fl.TurnTakingComplexity)
}

func TestAdaptive_FocusAndCarriedFields(t *testing.T) {
	in := specAdaptiveInput()
	in.Focus = nil
	got := learner.ComputeAdaptiveDifficulty(in)
	require.Equal(t, learner.AlgorithmVersion, got.AlgorithmVersion)
	require.Equal(t, 4, got.ScenarioDifficulty)
	require.Equal(t, 0, got.InSessionAdjustments)
	raw, err := json.Marshal(got)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"focus_skills":[]`)
}

func TestMaxFollowUps(t *testing.T) {
	for depth, want := range map[int]int{1: 0, 2: 1, 3: 2, 4: 3, 5: 3} {
		require.Equal(t, want, learner.MaxFollowUps(depth), depth)
	}
	require.Equal(t, 0, learner.MaxFollowUps(0))
	require.Equal(t, 3, learner.MaxFollowUps(9))
}

func TestRoundHalfUp(t *testing.T) {
	for x, want := range map[float64]int{0: 0, 0.49: 0, 0.5: 1, 1.5: 2, 2.5: 3, 2.4999: 2, 3.40: 3, 2.65: 3, 3.5: 4, -0.4: 0} {
		require.Equal(t, want, learner.RoundHalfUp(x), x)
	}
	// Float noise: 2.2+0.3 must round up like the exact 2.5.
	require.Equal(t, 3, learner.RoundHalfUp(2.2+0.3))
	require.Equal(t, 3, learner.RoundHalfUp((2.40+2.00)/2+0.3))
}

func TestAdaptive_RoundingSurvivesFloatNoise(t *testing.T) {
	// (0.01+4.39)/2+0.3 evaluates to 2.4999999999999996 in float64 but is exactly 2.50 -> R = 3.
	in := specAdaptiveInput() // B=4, bound [2,5]
	in.Levels[learner.SkillVocabulary], in.Levels[learner.SkillInteractionComplexity] = 0.01, 4.39
	require.Equal(t, 3, learner.ComputeAdaptiveDifficulty(in).TopicAbstraction)
}
