package learner

// AlgorithmVersion identifies the proficiency algorithm of spec 12.5. Changing any constant
// below MUST bump it: it is stored in proficiency_estimates.algorithm_version.
const AlgorithmVersion = 1

// Proficiency algorithm constants (spec 12.5).
const (
	MinTurnsForEvidence = 4
	PivotScore          = 70.0
	UpperSpan           = 30.0
	LowerSpan           = 35.0
	EvidenceBandBelow   = 2.0
	EvidenceBandAbove   = 1.0
	AlphaFloor          = 0.15
	MaxRisePerEval      = 0.40
	MaxDropPerEval      = 0.30
	ConfGainRate        = 0.25
	ConfPenaltyRate     = 0.15
	ConfPenaltyGap      = 1.0
	ConfMax             = 0.95
	ConfHalfLifeDays    = 90.0

	// DefaultEvalDimConfidence is q_eval when evaluations.dimension_confidence has no entry.
	DefaultEvalDimConfidence = 0.70

	TrendWindow     = 5
	TrendMinPoints  = 3
	TrendMaxAgeDays = 120
	TrendSlope      = 0.05

	// MinEvidenceConfidence is the c_obs below which a skill gets no estimate.
	MinEvidenceConfidence = 0.05
)
