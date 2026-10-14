package prompt

// LearnerContext is PromptData.Learner and component 6 of the budget. It is defined here so
// that prompt and content never import learner; package learner fills it (Bab 12 §12.8.4).
type LearnerContext struct {
	DisplayName string // learner_profiles.nickname; "" when NULL
	Adaptation  string // finished Japanese <conversation_difficulty> block, inserted verbatim

	// Render material (not read by templates):
	GoalLine    string
	FocusSkills []string // skill names, 0..2
}
