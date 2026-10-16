package learner

import "encoding/json"

// FindingStat counts the MAJOR and MINOR findings of one skill over the evaluations considered
// (spec 12.7).
type FindingStat struct{ Major, Minor int }

// AdaptiveInput is the input of ComputeAdaptiveDifficulty (spec 12.8.2). Focus is not in the
// spec struct but 12.8.3 needs it: it is the result of FocusSkills (12.7), filled by the caller.
// The spec field Findings is named FindingStats here.
type AdaptiveInput struct {
	Levels                map[Skill]float64 // EffectiveLevel (12.6), 6 skills
	Confidences           map[Skill]float64 // EffectiveConfidence
	Trends                map[Skill]string
	FindingStats          map[Skill]FindingStat
	EvaluationsConsidered int
	ScenarioDifficulty    int       // scenario_versions.difficulty_level 1..5
	ScenarioFormality     Formality // CASUAL | POLITE | KEIGO
	ScenarioFocus         []Skill   // scenario_versions.evaluation_focus
	VoiceAccent           string    // KANTO | KANSAI
	TopicSessions90d      int
	DialectSessions90d    int
	PrefSpeechSpeed       string // SLOWER | NORMAL | FASTER
	PrefFormality         string // AUTO | CASUAL | POLITE
	DialectPreference     string // STANDARD_ONLY | ALLOW_DIALECT
	Focus                 []Skill
}

// MarshalJSON writes a nil FocusSkills as [] so the stored snapshot never holds null.
func (a AdaptiveDifficulty) MarshalJSON() ([]byte, error) {
	type plain AdaptiveDifficulty
	p := plain(a)
	if p.FocusSkills == nil {
		p.FocusSkills = []Skill{}
	}
	return json.Marshal(p)
}
