package prompt

// VocabHint is one vocabulary hint of a scenario (alias of content.VocabHint's shape).
type VocabHint struct {
	TermJA  string `json:"term_ja"`
	Reading string `json:"reading"`
	Meaning string `json:"meaning"`
}

// CharacterData is the character part of the snapshot used in prompts (no voice, no vendor ids).
type CharacterData struct {
	Name         string
	Persona      string
	GreetingHint string // "" when NULL; only used by OPENING
}

// TopicData names the topic.
type TopicData struct {
	Code string
	Name string
}

// ScenarioData is the scenario version of the session.
type ScenarioData struct {
	Situation, AIRole, UserRole, Goal  string
	Tasks, Constraints, GrammarTargets []string
	VocabHints                         []VocabHint
	DifficultyLevel                    int
	DifficultyBand                     string
	OpeningLine                        string // used by OPENING; also available to CONVERSATION
}

// SpeechData is the speaking style (spec 6.3.6).
type SpeechData struct {
	Formality               string // CASUAL | POLITE | KEIGO_LIGHT | KEIGO
	FormalityInstruction    string
	MaxSentencesPerTurn     int
	MaxConsecutiveQuestions int
}

// MemoryData is the selected learner memory (Bab 11).
type MemoryData struct {
	Items    []string // selected non-episodic memories
	Episodes []string // top-3 episodic memories
}

// SessionData is the live session state.
type SessionData struct {
	Mode             string // USER_FIRST | AI_FIRST
	DurationMinutes  int
	RemainingMinutes int    // rounded up
	Summary          string // rolling summary; "" at the start
}

// PromptData is the data of the CONVERSATION and OPENING templates (spec 6.8.2). It carries no
// users.full_name, email, voice id or style/expression fields (S-4, D-014).
type PromptData struct { //nolint:revive // name fixed by the spec (6.8.2) and used by every milestone
	CoreRules      string // owned by code
	OutputContract string // owned by code (D-081)
	Character      CharacterData
	Topic          TopicData
	Scenario       ScenarioData
	Speech         SpeechData
	Learner        LearnerContext
	Memory         MemoryData
	Session        SessionData
}

// OpeningPromptData is the same struct, named for the OPENING key.
type OpeningPromptData = PromptData
