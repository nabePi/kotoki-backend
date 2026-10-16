package httpapi

import (
	"math"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/evaluation"
)

// evaluationFailureMessage is failure_message of a FAILED evaluation (spec 10.11).
const evaluationFailureMessage = "Evaluasi untuk sesi ini gagal diproses. Tim kami sudah diberi tahu."

type evaluationDimensionResponse struct {
	Dimension string  `json:"dimension"`
	Score     *int    `json:"score"`
	Comment   *string `json:"comment"`
}

type evaluationRecommendationResponse struct {
	Title     string  `json:"title"`
	Detail    string  `json:"detail"`
	ExampleJA *string `json:"example_ja"`
}

type evaluationFindingResponse struct {
	ID          uuid.UUID  `json:"id"`
	Dimension   string     `json:"dimension"`
	FindingType string     `json:"finding_type"`
	Severity    string     `json:"severity"`
	TurnID      *uuid.UUID `json:"turn_id"`
	TurnSeq     *int       `json:"turn_seq"`
	Original    *string    `json:"original"`
	Corrected   *string    `json:"corrected"`
	Explanation string     `json:"explanation"`
}

// evaluationResponse is the body of GET /sessions/{id}/evaluation (spec 10.11). It never carries
// provider, model, confidence, pattern_key, signals, token counts or last_error.
type evaluationResponse struct {
	SessionID            uuid.UUID                          `json:"session_id"`
	EvaluationID         *uuid.UUID                         `json:"evaluation_id"`
	Status               string                             `json:"status"`
	InsufficientEvidence bool                               `json:"insufficient_evidence"`
	DetailsPurged        bool                               `json:"details_purged"`
	RubricVersion        *string                            `json:"rubric_version"`
	OverallScore         *float64                           `json:"overall_score"`
	Dimensions           []evaluationDimensionResponse      `json:"dimensions"`
	Summary              *string                            `json:"summary"`
	Strengths            []string                           `json:"strengths"`
	FocusAreas           []string                           `json:"focus_areas"`
	Recommendations      []evaluationRecommendationResponse `json:"recommendations"`
	Findings             []evaluationFindingResponse        `json:"findings"`
	FailureMessage       *string                            `json:"failure_message"`
	CreatedAt            *string                            `json:"created_at"`
	CompletedAt          *string                            `json:"completed_at"`
}

var evaluationDimensionOrder = []evaluation.Dimension{
	evaluation.DimGrammar, evaluation.DimVocabulary, evaluation.DimFluency, evaluation.DimNaturalness, evaluation.DimTaskAchievement,
}

func newEvaluationResponse(v evaluation.View) evaluationResponse {
	out := evaluationResponse{
		SessionID: v.SessionID, Status: string(v.Status),
		Dimensions: make([]evaluationDimensionResponse, len(evaluationDimensionOrder)),
		Strengths:  []string{}, FocusAreas: []string{}, Recommendations: []evaluationRecommendationResponse{},
		Findings: []evaluationFindingResponse{},
	}
	for i, d := range evaluationDimensionOrder {
		out.Dimensions[i].Dimension = string(d)
	}
	e := v.Eval
	if e == nil {
		return out
	}
	id := e.ID
	rv := e.RubricVersion
	out.EvaluationID, out.RubricVersion = &id, &rv
	out.CreatedAt, out.CompletedAt = formatTimePtr(&e.CreatedAt), formatTimePtr(e.CompletedAt)

	switch e.Status {
	case evaluation.StatusFailed:
		msg := evaluationFailureMessage
		out.FailureMessage = &msg
		return out
	case evaluation.StatusCompleted:
	default:
		return out
	}

	out.InsufficientEvidence = e.InsufficientEvidence
	out.DetailsPurged = e.DetailsPurgedAt != nil
	out.OverallScore = roundScore(e.OverallScore)
	for i, p := range []*int{e.Scores.Grammar, e.Scores.Vocabulary, e.Scores.Fluency, e.Scores.Naturalness, e.Scores.TaskAchievement} {
		out.Dimensions[i].Score = p
	}
	if out.DetailsPurged {
		return out
	}
	out.Summary = e.SummaryFeedback
	for i, d := range evaluationDimensionOrder {
		if c, ok := e.DimensionComments[string(d)]; ok {
			out.Dimensions[i].Comment = &c
		}
	}
	out.Strengths = append(out.Strengths, e.Strengths...)
	out.FocusAreas = append(out.FocusAreas, e.FocusAreas...)
	for _, r := range e.Recommendations {
		out.Recommendations = append(out.Recommendations, evaluationRecommendationResponse{Title: r.Title, Detail: r.Detail, ExampleJA: r.ExampleJA})
	}
	for _, f := range v.Findings {
		fr := evaluationFindingResponse{
			ID: f.ID, Dimension: string(f.Dimension), FindingType: string(f.Type), Severity: string(f.Severity),
			TurnID: f.TurnID, TurnSeq: f.TurnSeq, Original: f.Original, Corrected: f.Corrected, Explanation: f.Explanation,
		}
		if v.TranscriptPurged {
			fr.TurnID = nil
		}
		out.Findings = append(out.Findings, fr)
	}
	return out
}

// roundScore rounds to two decimals (the column is NUMERIC(5,2); float noise must not leak).
func roundScore(p *float64) *float64 {
	if p == nil {
		return nil
	}
	r := math.Round(*p*100) / 100
	return &r
}
