package evaluation

// DimensionScore is one scored dimension of the LLM output (spec 10.7.1).
type DimensionScore struct {
	Score      int     `json:"score"`
	Confidence float64 `json:"confidence"`
	Comment    string  `json:"comment"`
}

// Signal is one auxiliary signal of the LLM output (not a scored dimension).
type Signal struct {
	Score      int     `json:"score"`
	Confidence float64 `json:"confidence"`
}

// Recommendation is one practice recommendation.
type Recommendation struct {
	Title     string  `json:"title"`
	Detail    string  `json:"detail"`
	ExampleJA *string `json:"example_ja"`
}

// RawFinding is a finding as the LLM produced it, before filtering (spec 10.7.1).
type RawFinding struct {
	Dimension   string  `json:"dimension"`
	FindingType string  `json:"finding_type"`
	Severity    string  `json:"severity"`
	TurnSeq     *int    `json:"turn_seq"`
	Original    *string `json:"original"`
	Corrected   *string `json:"corrected"`
	Explanation string  `json:"explanation"`
	PatternKey  *string `json:"pattern_key"`
	Confidence  float64 `json:"confidence"`
}

// EvalOutput is the parsed output of the rubric-v1 schema; its properties match the schema exactly.
type EvalOutput struct {
	Scores struct {
		Grammar         DimensionScore `json:"GRAMMAR"`
		Vocabulary      DimensionScore `json:"VOCABULARY"`
		Fluency         DimensionScore `json:"FLUENCY"`
		Naturalness     DimensionScore `json:"NATURALNESS"`
		TaskAchievement DimensionScore `json:"TASK_ACHIEVEMENT"`
	} `json:"scores"`
	Signals struct {
		ListeningResponse     Signal `json:"LISTENING_RESPONSE"`
		InteractionComplexity Signal `json:"INTERACTION_COMPLEXITY"`
	} `json:"signals"`
	Summary         string           `json:"summary"`
	Strengths       []string         `json:"strengths"`
	FocusAreas      []string         `json:"focus_areas"`
	Recommendations []Recommendation `json:"recommendations"`
	Findings        []RawFinding     `json:"findings"`
}

// Dimensions returns the five scored dimensions in LLMDimensions order.
func (o EvalOutput) Dimensions() map[Dimension]DimensionScore {
	return map[Dimension]DimensionScore{
		DimGrammar: o.Scores.Grammar, DimVocabulary: o.Scores.Vocabulary, DimFluency: o.Scores.Fluency,
		DimNaturalness: o.Scores.Naturalness, DimTaskAchievement: o.Scores.TaskAchievement,
	}
}
