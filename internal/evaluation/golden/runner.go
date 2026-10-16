package golden

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/content"
	"github.com/nabePi/kotoki-backend/internal/evaluation"
	"github.com/nabePi/kotoki-backend/internal/prompt"
	"github.com/nabePi/kotoki-backend/internal/provider"
)

// FindingKey is the part of a finding the calibration looks at.
type FindingKey struct {
	TurnSeq             int // 0 when the finding has no turn
	Type, Dimension     string
	Severity            string
	Original, Corrected string
}

// RunResult is one evaluation of one sample: the five dimension scores, the kept findings and
// whether the first LLM call already passed the validator.
type RunResult struct {
	Scores         map[string]int
	Findings       []FindingKey
	FirstCallValid bool
}

// Evaluator evaluates a sample; run is the 0-based repetition. An error aborts the calibration.
type Evaluator func(ctx context.Context, s Sample, run int) (RunResult, error)

// Metric statuses.
const (
	MetricPass          = "PASS"
	MetricFail          = "FAIL"
	MetricPendingManual = "PENDING_MANUAL"
	MetricReported      = "REPORTED"
)

// Verdicts of a report.
const (
	VerdictPass          = "PASS"
	VerdictFail          = "FAIL"
	VerdictNotEvaluable  = "NOT_EVALUABLE_EXAMPLE_DATASET"
	VerdictPendingManual = "PENDING_MANUAL_METRICS"
)

// Pass thresholds of spec 10.12.
const (
	thrMAE       = 10.0
	thrQWK       = 0.60
	thrSpearman  = 0.70
	thrStability = 5.0
	thrRecall    = 0.70
	thrValidity  = 0.95
	thrPrecision = 0.80
	qwkBands     = 5
)

// Metric keys of Report.Metrics; per-dimension metrics append "." and the dimension name.
const (
	MetricMAE            = "MAE"
	MetricQWKKey         = "QWK"
	MetricSpearmanKey    = "SPEARMAN"
	MetricMonotonic      = "MONOTONIC_LEVEL"
	MetricStability      = "STABILITY"
	MetricRecallMajor    = "RECALL_MAJOR"
	MetricFirstCallValid = "FIRST_CALL_VALIDITY"
	MetricPrecision      = "PRECISION"
	MetricInterRaterQWK  = "INTERRATER_QWK"
)

// MetricResult is one measured metric with its criterion. Op is "<=" or ">=" (empty for a metric that
// is only reported).
type MetricResult struct {
	Value     float64  `json:"value"`
	Threshold *float64 `json:"threshold,omitempty"`
	Op        string   `json:"op,omitempty"`
	Status    string   `json:"status"`
}

// Report is the outcome of a calibration run (report.json).
type Report struct {
	RubricVersion     string                  `json:"rubric_version"`
	DatasetStatus     string                  `json:"dataset_status"`
	Samples           int                     `json:"samples"`
	InsufficientCount int                     `json:"insufficient_samples"`
	Runs              int                     `json:"runs"`
	Metrics           map[string]MetricResult `json:"metrics"`
	LevelOverallMeans map[string]float64      `json:"level_overall_means"`
	Violations        []string                `json:"dataset_violations,omitempty"`
	Verdict           string                  `json:"verdict"`

	// Findings are the kept findings of every run, the pool ExportFindingsForRating draws from. They
	// carry user text, so they stay out of report.json.
	Findings []RatedFinding `json:"-"`
}

// RatedFinding is a kept finding with the sample it came from.
type RatedFinding struct {
	SampleID string
	Run      int
	Key      FindingKey
}

func metricMax(v, thr float64) MetricResult {
	return MetricResult{Value: round2(v), Threshold: &thr, Op: "<=", Status: statusOf(v <= thr)}
}

func metricMin(v, thr float64) MetricResult {
	return MetricResult{Value: round2(v), Threshold: &thr, Op: ">=", Status: statusOf(v >= thr)}
}

func statusOf(ok bool) string {
	if ok {
		return MetricPass
	}
	return MetricFail
}

// Run scores every non-insufficient sample runs times with ev and measures the 10.12 metrics. A
// dataset whose manifest says COMPLETE but violates the 80 + 8 shape is an error; an EXAMPLE dataset
// is measured too, but its verdict is never PASS.
func Run(ctx context.Context, ds Dataset, ev Evaluator, runs int) (Report, error) {
	runs = max(runs, 1)
	rep := Report{RubricVersion: ds.Manifest.RubricVersion, DatasetStatus: ds.Manifest.Status, Runs: runs,
		Metrics: map[string]MetricResult{}, LevelOverallMeans: map[string]float64{}, Violations: ds.CheckComplete()}
	if ds.Manifest.Status == StatusComplete && len(rep.Violations) > 0 {
		return rep, fmt.Errorf("golden: manifest says COMPLETE but the dataset is not: %v", rep.Violations)
	}

	type scored struct {
		s    Sample
		gold map[string]float64
		runs []RunResult
	}
	var all []scored
	for _, s := range ds.Samples {
		if s.Insufficient {
			rep.InsufficientCount++
			continue
		}
		item := scored{s: s, gold: map[string]float64{}}
		for _, d := range evaluation.LLMDimensions {
			g, err := s.Gold.Score(string(d))
			if err != nil {
				return rep, fmt.Errorf("golden: sample %s: %w", s.ID, err)
			}
			item.gold[string(d)] = g
		}
		for r := range runs {
			if err := ctx.Err(); err != nil {
				return rep, err
			}
			res, err := ev(ctx, s, r)
			if err != nil {
				return rep, fmt.Errorf("golden: sample %s run %d: %w", s.ID, r+1, err)
			}
			for _, d := range evaluation.LLMDimensions {
				if _, ok := res.Scores[string(d)]; !ok {
					return rep, fmt.Errorf("golden: sample %s run %d: no score for %s", s.ID, r+1, d)
				}
			}
			for _, f := range res.Findings {
				rep.Findings = append(rep.Findings, RatedFinding{SampleID: s.ID, Run: r + 1, Key: f})
			}
			item.runs = append(item.runs, res)
		}
		all = append(all, item)
	}
	rep.Samples = len(all)
	if len(all) == 0 {
		return rep, errors.New("golden: dataset has no scored samples")
	}

	// model mean per sample and dimension over the runs
	modelMean := func(it scored, d string) float64 {
		xs := make([]float64, 0, len(it.runs))
		for _, r := range it.runs {
			xs = append(xs, float64(r.Scores[d]))
		}
		return mean(xs)
	}

	var stdDevs []float64
	for _, d := range evaluation.LLMDimensions {
		dim := string(d)
		model, gold := make([]float64, 0, len(all)), make([]float64, 0, len(all))
		mb, gb := make([]int, 0, len(all)), make([]int, 0, len(all))
		for _, it := range all {
			m := modelMean(it, dim)
			model, gold = append(model, m), append(gold, it.gold[dim])
			mb, gb = append(mb, Band(m)), append(gb, Band(it.gold[dim]))
			per := make([]float64, 0, len(it.runs))
			for _, r := range it.runs {
				per = append(per, float64(r.Scores[dim]))
			}
			stdDevs = append(stdDevs, StdDev(per))
		}
		rep.Metrics[MetricMAE+"."+dim] = metricMax(MAE(model, gold), thrMAE)
		rep.Metrics[MetricQWKKey+"."+dim] = metricMin(QWK(mb, gb, qwkBands), thrQWK)
		rep.Metrics[MetricSpearmanKey+"."+dim] = metricMin(Spearman(model, gold), thrSpearman)
	}
	rep.Metrics[MetricStability] = metricMax(mean(stdDevs), thrStability)

	// monotonic overall per level
	byLevel := map[string][]float64{}
	for _, it := range all {
		var dims []float64
		for _, d := range evaluation.LLMDimensions {
			dims = append(dims, modelMean(it, string(d)))
		}
		byLevel[it.s.Level] = append(byLevel[it.s.Level], mean(dims))
	}
	var means []float64
	allLevels := true
	for _, l := range Levels {
		if len(byLevel[l]) == 0 {
			allLevels = false
			continue
		}
		m := mean(byLevel[l])
		rep.LevelOverallMeans[l] = round2(m)
		means = append(means, m)
	}
	mono := allLevels && StrictlyIncreasing(means)
	monoValue := 0.0
	if mono {
		monoValue = 1
	}
	rep.Metrics[MetricMonotonic] = MetricResult{Value: monoValue, Status: statusOf(mono)}

	// recall of gold MAJOR errors and first-call validity, over (item, run) pairs
	var found, total, valid, calls int
	for _, it := range all {
		for _, r := range it.runs {
			calls++
			if r.FirstCallValid {
				valid++
			}
			for _, me := range it.s.Gold.MajorErrors {
				total++
				if majorFound(me, r.Findings) {
					found++
				}
			}
		}
	}
	rep.Metrics[MetricRecallMajor] = metricMin(Recall(found, total), thrRecall)
	rep.Metrics[MetricFirstCallValid] = metricMin(Recall(valid, calls), thrValidity)

	// agreement between the two raters, pooled over all dimensions (reported only)
	var r1, r2 []int
	for _, it := range all {
		for _, d := range evaluation.LLMDimensions {
			r1 = append(r1, Band(float64(it.s.Gold.Rater1[string(d)])))
			r2 = append(r2, Band(float64(it.s.Gold.Rater2[string(d)])))
		}
	}
	rep.Metrics[MetricInterRaterQWK] = MetricResult{Value: round2(QWK(r1, r2, qwkBands)), Status: MetricReported}

	thr := thrPrecision
	rep.Metrics[MetricPrecision] = MetricResult{Threshold: &thr, Op: ">=", Status: MetricPendingManual}
	rep.Verdict = rep.verdict()
	return rep, nil
}

// majorFound reports whether a finding sits on the turn of the gold error with the same finding type
// or the same dimension (spec 10.12).
func majorFound(me MajorError, findings []FindingKey) bool {
	wantDim := string(evaluation.FindingDimension[evaluation.FindingType(me.FindingType)])
	for _, f := range findings {
		if f.TurnSeq != me.TurnSeq {
			continue
		}
		if f.Type == me.FindingType || (wantDim != "" && f.Dimension == wantDim) {
			return true
		}
	}
	return false
}

// verdict derives the verdict from the dataset status and the metric statuses.
func (r Report) verdict() string {
	if r.DatasetStatus != StatusComplete {
		return VerdictNotEvaluable
	}
	pending := false
	for _, m := range r.Metrics {
		switch m.Status {
		case MetricFail:
			return VerdictFail
		case MetricPendingManual:
			pending = true
		}
	}
	if pending {
		return VerdictPendingManual
	}
	return VerdictPass
}

// NewLLMEvaluator returns the Evaluator of the real pipeline: it builds the prompt exactly like the
// worker (BuildTranscript, BuildPromptData, prompt.Render of the published EVALUATION version pv) and
// runs evaluation.LLMEvaluator against ev. FirstCallValid is CallCount == 1; an evaluation that stays
// invalid after the repair call is an error.
func NewLLMEvaluator(pv content.PromptVersion, ev provider.EvaluationProvider, schemas *evaluation.SchemaCache, minConfidence float64) Evaluator {
	llm := evaluation.NewLLMEvaluator(ev, schemas, nil, minConfidence)
	maxChars := evaluation.DefaultConfig().TranscriptMaxChars
	return func(ctx context.Context, s Sample, _ int) (RunResult, error) {
		sv := content.ScenarioVersion{
			DifficultyLevel: s.Scenario.DifficultyLevel, Situation: s.Scenario.Situation, AIRole: s.Scenario.AIRole,
			UserRole: s.Scenario.UserRole, Goal: s.Scenario.Goal, Tasks: s.Scenario.Tasks,
			GrammarTargets: s.Scenario.GrammarTargets, EvaluationFocus: s.Scenario.EvaluationFocus,
			Formality: content.Formality(s.Scenario.Formality),
		}
		data := evaluation.BuildPromptData(evaluation.PromptInput{
			TopicName: s.Scenario.Topic, ScenarioName: s.Scenario.Title, CharacterName: s.Scenario.Character, SV: sv,
			RubricVersion: evaluation.RubricVersionOf(pv), Stats: s.Stats(),
			Transcript: evaluation.BuildTranscript(s.Turns, maxChars),
		})
		rendered, err := prompt.Render(pv.Template(), data)
		if err != nil {
			return RunResult{}, err
		}
		refs := make([]evaluation.TurnRef, 0, len(s.Turns))
		for _, t := range s.Turns {
			if t.UserText != nil {
				refs = append(refs, evaluation.TurnRef{ID: turnID(s.ID, t.Seq), Seq: t.Seq, UserText: *t.UserText})
			}
		}
		res, err := llm.Evaluate(ctx, evaluation.LLMInput{SessionID: sampleSessionID(s.ID), PV: pv, Rendered: rendered, Turns: refs})
		if err != nil {
			return RunResult{}, err
		}
		out := RunResult{Scores: map[string]int{}, FirstCallValid: res.CallCount == 1}
		for d, sc := range res.Output.Dimensions() {
			out.Scores[string(d)] = sc.Score
		}
		for _, f := range res.Findings {
			k := FindingKey{Type: string(f.Type), Dimension: string(f.Dimension), Severity: string(f.Severity)}
			if f.TurnSeq != nil {
				k.TurnSeq = *f.TurnSeq
			}
			if f.Original != nil {
				k.Original = *f.Original
			}
			if f.Corrected != nil {
				k.Corrected = *f.Corrected
			}
			out.Findings = append(out.Findings, k)
		}
		return out, nil
	}
}

// goldenNamespace derives stable ids for samples, so a rerun sends the same RequestID.
var goldenNamespace = uuid.MustParse("6f1c2b0e-5d7a-4c3e-9a41-0b6f8e2d7c11")

func sampleSessionID(id string) uuid.UUID { return uuid.NewSHA1(goldenNamespace, []byte(id)) }

func turnID(sampleID string, seq int) uuid.UUID {
	return uuid.NewSHA1(goldenNamespace, []byte(fmt.Sprintf("%s#%d", sampleID, seq)))
}
