package learner_test

import (
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/learner"
	"github.com/nabePi/kotoki-backend/internal/prompt"
)

func exampleAD() learner.AdaptiveDifficulty {
	return learner.ComputeAdaptiveDifficulty(specAdaptiveInput())
}

func TestRenderAdaptation_Layout(t *testing.T) { // spec 12.8.3 example + goal JOB_INTERVIEW
	out := learner.RenderAdaptation(prompt.GoalDescriptor("JOB_INTERVIEW"), exampleAD())
	lines := strings.Split(out, "\n")
	// header, goal, 10 dimensions, scaffolding, formality, dialect, sentences, ねらい + 2, closing, footer
	require.Len(t, lines, 21)
	require.Equal(t, "<conversation_difficulty>", lines[0])
	require.Equal(t, "学習者の目標: 日本での就職面接に向けた練習。", lines[1])
	for i, dim := range []string{"vocabulary", "grammar", "speech_speed", "topic_novelty", "topic_abstraction",
		"question_complexity", "follow_up_depth", "expected_response_length", "spontaneity", "turn_taking"} {
		level := map[string]int{"vocabulary": 3, "grammar": 3, "speech_speed": 3, "topic_novelty": 4,
			"topic_abstraction": 3, "question_complexity": 3, "follow_up_depth": 2,
			"expected_response_length": 3, "spontaneity": 3, "turn_taking": 3}[dim]
		require.Equal(t, "- "+prompt.DimensionDescriptor(dim, level), lines[2+i], dim)
	}
	require.Equal(t, "- "+prompt.ScaffoldingDescriptor(3), lines[12])
	require.Equal(t, "- "+prompt.FormalityDescriptor("KEIGO_LIGHT"), lines[13])
	require.Equal(t, "- "+prompt.DialectDescriptor("NONE"), lines[14])
	require.Equal(t, "- 一回の発話は3文以内にする。", lines[15])
	require.Equal(t, "学習のねらい:", lines[len(lines)-5])
	require.Equal(t, "- "+prompt.FocusDescriptor("GRAMMAR"), lines[len(lines)-4])
	require.Equal(t, "- "+prompt.FocusDescriptor("NATURALNESS"), lines[len(lines)-3])
	require.Equal(t, prompt.ClosingLine, lines[len(lines)-2])
	require.Equal(t, "</conversation_difficulty>", lines[len(lines)-1])
	require.False(t, strings.HasSuffix(out, "\n"))
}

func TestRenderAdaptation_NoFocusNoGoal_OmitsHeaders(t *testing.T) {
	ad := exampleAD()
	ad.FocusSkills = nil
	out := learner.RenderAdaptation("", ad)
	require.NotContains(t, out, "学習者の目標")
	require.NotContains(t, out, "学習のねらい:")
	lines := strings.Split(out, "\n")
	require.Len(t, lines, 17)
	require.Equal(t, "<conversation_difficulty>", lines[0])
	require.True(t, strings.HasPrefix(lines[1], "- "))
	require.Equal(t, prompt.ClosingLine, lines[len(lines)-2])
}

func TestRenderAdaptation_OneFocus(t *testing.T) {
	ad := exampleAD()
	ad.FocusSkills = []learner.Skill{learner.SkillFluency}
	lines := strings.Split(learner.RenderAdaptation("", ad), "\n")
	require.Equal(t, "学習のねらい:", lines[len(lines)-4])
	require.Equal(t, "- "+prompt.FocusDescriptor("FLUENCY"), lines[len(lines)-3])
}

func TestRenderAdaptation_UsesSentenceCapAndEveryDimension(t *testing.T) {
	ad := exampleAD()
	ad.MaxAISentencesPerTurn = 4
	require.Contains(t, learner.RenderAdaptation("", ad), "一回の発話は4文以内にする。")
	// Each dimension reads its own field: bump one at a time and expect exactly one line to change.
	base := strings.Split(learner.RenderAdaptation("", exampleAD()), "\n")
	bump := map[string]func(*learner.AdaptiveDifficulty){
		"vocabulary":  func(a *learner.AdaptiveDifficulty) { a.VocabularyComplexity = 5 },
		"grammar":     func(a *learner.AdaptiveDifficulty) { a.GrammarComplexity = 5 },
		"speed":       func(a *learner.AdaptiveDifficulty) { a.SpeechSpeed = 5 },
		"novelty":     func(a *learner.AdaptiveDifficulty) { a.TopicNovelty = 1 },
		"abstraction": func(a *learner.AdaptiveDifficulty) { a.TopicAbstraction = 5 },
		"question":    func(a *learner.AdaptiveDifficulty) { a.QuestionComplexity = 5 },
		"followup":    func(a *learner.AdaptiveDifficulty) { a.FollowUpDepth = 5 },
		"length":      func(a *learner.AdaptiveDifficulty) { a.ExpectedResponseLength = 5 },
		"spont":       func(a *learner.AdaptiveDifficulty) { a.Spontaneity = 5 },
		"turn":        func(a *learner.AdaptiveDifficulty) { a.TurnTakingComplexity = 5 },
		"scaffolding": func(a *learner.AdaptiveDifficulty) { a.Scaffolding = 0 },
		"formality":   func(a *learner.AdaptiveDifficulty) { a.Formality = learner.FormalityCasual },
		"dialect":     func(a *learner.AdaptiveDifficulty) { a.DialectExposure = learner.DialectFull },
	}
	for name, f := range bump {
		ad := exampleAD()
		f(&ad)
		got := strings.Split(learner.RenderAdaptation("", ad), "\n")
		require.Len(t, got, len(base), name)
		diff := 0
		for i := range got {
			if got[i] != base[i] {
				diff++
			}
		}
		require.Equal(t, 1, diff, name)
	}
}

// worstCaseAD picks the longest descriptor of every dimension.
func worstCaseAD() learner.AdaptiveDifficulty {
	longest := func(dim string) int {
		best, bestN := 1, 0
		for l := 1; l <= 5; l++ {
			if n := utf8.RuneCountInString(prompt.DimensionDescriptor(dim, l)); n > bestN {
				best, bestN = l, n
			}
		}
		return best
	}
	longestOf := func(vals []string, get func(string) string) string {
		best, bestN := "", -1
		for _, v := range vals {
			if n := utf8.RuneCountInString(get(v)); n > bestN {
				best, bestN = v, n
			}
		}
		return best
	}
	sc := 0
	for i := 0; i <= 3; i++ {
		if utf8.RuneCountInString(prompt.ScaffoldingDescriptor(i)) > utf8.RuneCountInString(prompt.ScaffoldingDescriptor(sc)) {
			sc = i
		}
	}
	return learner.AdaptiveDifficulty{
		VocabularyComplexity: longest("vocabulary"), GrammarComplexity: longest("grammar"),
		SpeechSpeed: longest("speech_speed"), TopicNovelty: longest("topic_novelty"),
		TopicAbstraction: longest("topic_abstraction"), QuestionComplexity: longest("question_complexity"),
		FollowUpDepth: longest("follow_up_depth"), ExpectedResponseLength: longest("expected_response_length"),
		Spontaneity: longest("spontaneity"), TurnTakingComplexity: longest("turn_taking"),
		Scaffolding:           sc,
		Formality:             learner.Formality(longestOf([]string{"CASUAL", "POLITE", "KEIGO_LIGHT", "KEIGO"}, prompt.FormalityDescriptor)),
		DialectExposure:       learner.DialectExposure(longestOf([]string{"NONE", "LIGHT", "FULL"}, prompt.DialectDescriptor)),
		MaxAISentencesPerTurn: 4,
		FocusSkills:           []learner.Skill{learner.SkillGrammar, learner.SkillVocabulary}, // replaced below
	}
}

func TestLearnerContextWorstCaseBudget(t *testing.T) {
	ad := worstCaseAD()
	// The two longest focus descriptors.
	type fd struct {
		s learner.Skill
		n int
	}
	var all []fd
	for _, s := range learner.AllSkills() {
		all = append(all, fd{s, utf8.RuneCountInString(prompt.FocusDescriptor(string(s)))})
	}
	for i := range all {
		for j := i + 1; j < len(all); j++ {
			if all[j].n > all[i].n {
				all[i], all[j] = all[j], all[i]
			}
		}
	}
	ad.FocusSkills = []learner.Skill{all[0].s, all[1].s}
	goals := []string{"DAILY_CONVERSATION", "JOB_INTERVIEW", "BUSINESS", "TRAVEL", "EXAM_PREP", "LIVING_IN_JAPAN"}
	goal := goals[0]
	for _, g := range goals {
		if utf8.RuneCountInString(prompt.GoalDescriptor(g)) > utf8.RuneCountInString(prompt.GoalDescriptor(goal)) {
			goal = g
		}
	}
	nick := strings.Repeat("あ", 30)
	ctx := learner.NewPromptContext(learner.ProfileSnapshot{Nickname: &nick, LearningGoal: &goal}, ad)

	focusBlock := "学習のねらい:\n- " + prompt.FocusDescriptor(string(ad.FocusSkills[0])) + "\n- " + prompt.FocusDescriptor(string(ad.FocusSkills[1])) + "\n"
	require.Contains(t, ctx.Adaptation, focusBlock)
	// Spec 12.8.4: worst case fits the 700-token learner component whole (measured: ~659), so the
	// plan's relaxed bound (plus the 学習のねらい block) is not needed and the strict one is kept.
	require.LessOrEqual(t, prompt.EstimateTokens(ctx.Adaptation+ctx.DisplayName), prompt.DefaultBudget.Learner)
	require.LessOrEqual(t, utf8.RuneCountInString(ctx.Adaptation), 800)
}

func TestNewPromptContext_Fields(t *testing.T) {
	ad := exampleAD()
	ctx := learner.NewPromptContext(learner.ProfileSnapshot{Nickname: strp("Budi"), LearningGoal: strp("JOB_INTERVIEW")}, ad)
	require.Equal(t, "Budi", ctx.DisplayName)
	require.Equal(t, "日本での就職面接に向けた練習", ctx.GoalLine)
	require.Equal(t, []string{"GRAMMAR", "NATURALNESS"}, ctx.FocusSkills)
	require.Equal(t, learner.RenderAdaptation(ctx.GoalLine, ad), ctx.Adaptation)

	for _, goal := range []*string{nil, strp("OTHER"), strp("")} {
		c := learner.NewPromptContext(learner.ProfileSnapshot{LearningGoal: goal}, ad)
		require.Equal(t, "", c.GoalLine)
		require.NotContains(t, c.Adaptation, "学習者の目標")
	}

	ad.FocusSkills = nil
	c := learner.NewPromptContext(learner.ProfileSnapshot{}, ad)
	require.NotNil(t, c.FocusSkills)
	require.Empty(t, c.FocusSkills)
}

func TestPromptContext_NeverContainsFullName(t *testing.T) {
	// ProfileSnapshot carries no full name field, and NewPromptContext takes exactly the
	// snapshot and the adaptive difficulty.
	pt := reflect.TypeOf(learner.ProfileSnapshot{})
	var fields []string
	for i := 0; i < pt.NumField(); i++ {
		fields = append(fields, pt.Field(i).Name)
	}
	require.ElementsMatch(t, []string{"Nickname", "LearningGoal"}, fields)
	ft := reflect.TypeOf(learner.NewPromptContext)
	require.Equal(t, 2, ft.NumIn())
	require.Equal(t, pt, ft.In(0))

	c := learner.NewPromptContext(learner.ProfileSnapshot{}, exampleAD())
	require.Equal(t, "", c.DisplayName)
}

func TestRerender_KeepsDisplayNameAndGoal_ReplacesAdaptation(t *testing.T) {
	ad := exampleAD()
	prev := learner.NewPromptContext(learner.ProfileSnapshot{Nickname: strp("Budi"), LearningGoal: strp("TRAVEL")}, ad)
	ad2 := ad
	ad2.Scaffolding = 2
	ad2.FocusSkills = []learner.Skill{learner.SkillFluency}
	got := learner.Rerender(prev, ad2)
	require.Equal(t, "Budi", got.DisplayName)
	require.Equal(t, prev.GoalLine, got.GoalLine)
	require.NotEqual(t, prev.Adaptation, got.Adaptation)
	require.Equal(t, learner.RenderAdaptation(prev.GoalLine, ad2), got.Adaptation)
	require.Equal(t, []string{"FLUENCY"}, got.FocusSkills)
	require.Equal(t, []string{"GRAMMAR", "NATURALNESS"}, prev.FocusSkills, "prev is not mutated")
}
