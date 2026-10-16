package learner

// The six skills of the learner profile, in the fixed D-097 order.
const (
	SkillGrammar               Skill = "GRAMMAR"
	SkillVocabulary            Skill = "VOCABULARY"
	SkillFluency               Skill = "FLUENCY"
	SkillNaturalness           Skill = "NATURALNESS"
	SkillListeningResponse     Skill = "LISTENING_RESPONSE"
	SkillInteractionComplexity Skill = "INTERACTION_COMPLEXITY"
)

var allSkills = [...]Skill{
	SkillGrammar, SkillVocabulary, SkillFluency,
	SkillNaturalness, SkillListeningResponse, SkillInteractionComplexity,
}

// AllSkills returns the six skills in D-097 order. The slice is a fresh copy.
func AllSkills() []Skill {
	out := make([]Skill, len(allSkills))
	copy(out, allSkills[:])
	return out
}

// ParseSkill maps a skill name to its Skill; the second result is false for unknown names.
func ParseSkill(s string) (Skill, bool) {
	for _, k := range allSkills {
		if string(k) == s {
			return k, true
		}
	}
	return "", false
}
