package learner

import (
	"strings"

	"github.com/nabePi/kotoki-backend/internal/prompt"
)

// ProfileSnapshot is the part of learner_profiles the prompt needs. users.full_name is
// deliberately absent: it must never reach a prompt (spec 12.8.4).
type ProfileSnapshot struct{ Nickname, LearningGoal *string }

// RenderAdaptation renders the <conversation_difficulty> block (spec 12.8.4). goalDescriptor is
// prompt.GoalDescriptor of the learning goal; "" omits the goal line.
func RenderAdaptation(goalDescriptor string, ad AdaptiveDifficulty) string {
	var b strings.Builder
	line := func(s string) {
		if s != "" {
			b.WriteString(s)
			b.WriteByte('\n')
		}
	}
	item := func(s string) {
		if s != "" {
			line("- " + s)
		}
	}
	line("<conversation_difficulty>")
	if goalDescriptor != "" {
		line("学習者の目標: " + goalDescriptor + "。")
	}
	levels := map[string]int{
		"vocabulary": ad.VocabularyComplexity, "grammar": ad.GrammarComplexity,
		"speech_speed": ad.SpeechSpeed, "topic_novelty": ad.TopicNovelty,
		"topic_abstraction": ad.TopicAbstraction, "question_complexity": ad.QuestionComplexity,
		"follow_up_depth": ad.FollowUpDepth, "expected_response_length": ad.ExpectedResponseLength,
		"spontaneity": ad.Spontaneity, "turn_taking": ad.TurnTakingComplexity,
	}
	for _, dim := range prompt.DimensionDescriptors {
		item(prompt.DimensionDescriptor(dim, levels[dim]))
	}
	item(prompt.ScaffoldingDescriptor(ad.Scaffolding))
	item(prompt.FormalityDescriptor(string(ad.Formality)))
	item(prompt.DialectDescriptor(string(ad.DialectExposure)))
	item(prompt.MaxSentencesLine(ad.MaxAISentencesPerTurn))
	var focus []string
	for _, s := range ad.FocusSkills {
		if d := prompt.FocusDescriptor(string(s)); d != "" {
			focus = append(focus, d)
		}
	}
	if len(focus) > 0 {
		line("学習のねらい:")
		for _, d := range focus {
			item(d)
		}
	}
	line(prompt.ClosingLine)
	b.WriteString("</conversation_difficulty>")
	return b.String()
}

func focusNames(ad AdaptiveDifficulty) []string {
	out := make([]string, 0, len(ad.FocusSkills))
	for _, s := range ad.FocusSkills {
		out = append(out, string(s))
	}
	return out
}

// NewPromptContext builds prompt.LearnerContext from the profile snapshot and the session's
// adaptive difficulty. Pure; it takes no full_name by construction.
func NewPromptContext(p ProfileSnapshot, ad AdaptiveDifficulty) prompt.LearnerContext {
	var name, goal string
	if p.Nickname != nil {
		name = *p.Nickname
	}
	if p.LearningGoal != nil {
		goal = prompt.GoalDescriptor(*p.LearningGoal)
	}
	return prompt.LearnerContext{
		DisplayName: name,
		GoalLine:    goal,
		FocusSkills: focusNames(ad),
		Adaptation:  RenderAdaptation(goal, ad),
	}
}

// Rerender recomputes Adaptation and FocusSkills after an in-session adjustment, keeping
// DisplayName and GoalLine of prev (conversation.LearnerContextProvider.Rerender).
func Rerender(prev prompt.LearnerContext, ad AdaptiveDifficulty) prompt.LearnerContext {
	prev.FocusSkills = focusNames(ad)
	prev.Adaptation = RenderAdaptation(prev.GoalLine, ad)
	return prev
}
