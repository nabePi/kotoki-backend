// Package evaluation owns the post-session evaluation rules (spec Bab 10): enums, finding specs,
// scoring, transcript, prompt data, LLM call sequence, orchestration and the consumer.
package evaluation

import (
	"fmt"

	"github.com/nabePi/kotoki-backend/internal/content"
)

// Status is the lifecycle of an evaluation (spec 10.3).
type Status string

// Evaluation statuses.
const (
	StatusPending   Status = "PENDING"
	StatusRunning   Status = "RUNNING"
	StatusCompleted Status = "COMPLETED"
	StatusFailed    Status = "FAILED"
)

// Dimension is one of the five scored dimensions (D-096).
type Dimension string

// Dimensions.
const (
	DimGrammar         Dimension = "GRAMMAR"
	DimVocabulary      Dimension = "VOCABULARY"
	DimFluency         Dimension = "FLUENCY"
	DimNaturalness     Dimension = "NATURALNESS"
	DimTaskAchievement Dimension = "TASK_ACHIEVEMENT"
)

// Severity of a finding (D-096).
type Severity string

// Severities.
const (
	SeverityInfo  Severity = "INFO"
	SeverityMinor Severity = "MINOR"
	SeverityMajor Severity = "MAJOR"
)

// FindingType is one of the 16 finding types (spec 10.1).
type FindingType string

// Finding types, in the order of the spec table.
const (
	FindingParticleError          FindingType = "PARTICLE_ERROR"
	FindingConjugationError       FindingType = "CONJUGATION_ERROR"
	FindingTenseAspectError       FindingType = "TENSE_ASPECT_ERROR"
	FindingSentenceStructureError FindingType = "SENTENCE_STRUCTURE_ERROR"
	FindingWordChoiceError        FindingType = "WORD_CHOICE_ERROR"
	FindingNonJapaneseWord        FindingType = "NON_JAPANESE_WORD"
	FindingCollocationError       FindingType = "COLLOCATION_ERROR"
	FindingPolitenessMismatch     FindingType = "POLITENESS_MISMATCH"
	FindingUnnaturalExpression    FindingType = "UNNATURAL_EXPRESSION"
	FindingExcessiveFiller        FindingType = "EXCESSIVE_FILLER"
	FindingFragmentedUtterance    FindingType = "FRAGMENTED_UTTERANCE"
	FindingOverlyShortResponse    FindingType = "OVERLY_SHORT_RESPONSE"
	FindingOffTopicResponse       FindingType = "OFF_TOPIC_RESPONSE"
	FindingMisunderstoodPrompt    FindingType = "MISUNDERSTOOD_PROMPT"
	FindingObjectiveNotMet        FindingType = "OBJECTIVE_NOT_MET"
	FindingStrength               FindingType = "STRENGTH"
)

// FailureCode is evaluations.failure_code for a FAILED evaluation.
type FailureCode string

// Failure codes.
const (
	FailTranscriptUnavailable FailureCode = "TRANSCRIPT_UNAVAILABLE"
	FailProviderUnavailable   FailureCode = "PROVIDER_UNAVAILABLE"
	FailInvalidModelOutput    FailureCode = "INVALID_MODEL_OUTPUT"
	FailConfigurationError    FailureCode = "CONFIGURATION_ERROR"
	FailInternalError         FailureCode = "INTERNAL_ERROR"
)

// RubricVersionOf is the rubric version of the EVALUATION prompt version used (S-1), e.g.
// "rubric-v1".
func RubricVersionOf(pv content.PromptVersion) string {
	return fmt.Sprintf("rubric-v%d", pv.Version)
}

// FindingDimension maps every finding type except STRENGTH (any of the five dimensions) to its
// dimension.
var FindingDimension = func() map[FindingType]Dimension {
	m := map[FindingType]Dimension{}
	for _, t := range AllFindingTypes {
		if s := specs[t]; t != FindingStrength {
			m[t] = s.Dimension
		}
	}
	return m
}()

// LLMDimensions are the five dimensions in the D-096 order.
var LLMDimensions = []Dimension{DimGrammar, DimVocabulary, DimFluency, DimNaturalness, DimTaskAchievement}
