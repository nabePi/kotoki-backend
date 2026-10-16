package golden

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/nabePi/kotoki-backend/internal/evaluation"
)

// Manifest statuses.
const (
	StatusExample  = "EXAMPLE"
	StatusComplete = "COMPLETE"
)

// Levels of the golden set in ascending order (spec 10.12).
var Levels = []string{"BEGINNER", "ELEMENTARY", "INTERMEDIATE", "ADVANCED"}

// Start modes of a session (D-056).
var startModes = []string{"USER_FIRST", "AI_FIRST"}

// Defaults of Manifest.Expected for a manifest that leaves a count out (spec 10.12).
const (
	defaultSamples          = 80
	defaultPerLevel         = 20
	defaultInsufficient     = 8
	defaultMinScenarioKinds = 4
	minUserTurns            = 6
	maxUserTurns            = 15
	// adjudicationGap is the largest rater difference accepted without a third rater.
	adjudicationGap = 20
)

// ErrAdjudicationMissing means two raters differ by more than 20 points on a dimension and no third
// rater adjudicated it (spec 10.12).
var ErrAdjudicationMissing = errors.New("golden: raters differ by more than 20 points and rater3 is missing")

// Manifest describes a dataset directory (manifest.json). Status EXAMPLE marks the synthetic sample
// set, which can never produce a PASS; COMPLETE asserts the real 80 + 8 set.
type Manifest struct {
	RubricVersion string `json:"rubric_version"`
	Status        string `json:"status"`
	Note          string `json:"note,omitempty"`
	Expected      struct {
		Samples          int `json:"samples"`
		PerLevel         int `json:"per_level"`
		Insufficient     int `json:"insufficient"`
		MinScenarioKinds int `json:"min_scenario_kinds"`
	} `json:"expected"`
}

// ScenarioInput is the scenario context of a sample, the part of the worker's prompt data that
// comes from the scenario version, topic and character.
type ScenarioInput struct {
	Kind            string   `json:"kind"` // daily, job_interview, culture, free_talk ...: only counted per level
	Topic           string   `json:"topic"`
	Title           string   `json:"title"`
	Character       string   `json:"character"`
	DifficultyLevel int      `json:"difficulty_level"`
	UserRole        string   `json:"user_role"`
	AIRole          string   `json:"ai_role"`
	Situation       string   `json:"situation"`
	Goal            string   `json:"goal"`
	Tasks           []string `json:"tasks"`
	GrammarTargets  []string `json:"grammar_targets"`
	EvaluationFocus []string `json:"evaluation_focus"`
	Formality       string   `json:"formality"` // CASUAL, POLITE, KEIGO_LIGHT or KEIGO
}

// MajorError is a gold MAJOR mistake the evaluator must report.
type MajorError struct {
	TurnSeq     int    `json:"turn_seq"`
	Original    string `json:"original"`
	FindingType string `json:"finding_type"`
}

// Gold holds the rater labels of one sample: per-dimension scores of each rater and the MAJOR
// errors that must be detected.
type Gold struct {
	Rater1      map[string]int `json:"rater1"`
	Rater2      map[string]int `json:"rater2"`
	Rater3      map[string]int `json:"rater3,omitempty"`
	MajorErrors []MajorError   `json:"major_errors"`
}

// Score is the gold score of dim: the mean of rater 1 and 2; when they differ by more than 20 points
// rater 3 adjudicates and the gold is the mean of the two closest raters (the plan's reading of
// spec 10.12, which does not detail the adjudication), and without rater 3 the answer is
// ErrAdjudicationMissing.
func (g Gold) Score(dim string) (float64, error) {
	a, okA := g.Rater1[dim]
	b, okB := g.Rater2[dim]
	if !okA || !okB {
		return 0, fmt.Errorf("golden: rater1/rater2 score for %s is missing", dim)
	}
	if abs(a-b) <= adjudicationGap {
		return float64(a+b) / 2, nil
	}
	c, okC := g.Rater3[dim]
	if !okC {
		return 0, fmt.Errorf("%w (%s)", ErrAdjudicationMissing, dim)
	}
	pairs := [][2]int{{a, b}, {a, c}, {b, c}}
	best := pairs[0]
	for _, p := range pairs[1:] {
		if abs(p[0]-p[1]) < abs(best[0]-best[1]) {
			best = p
		}
	}
	return float64(best[0]+best[1]) / 2, nil
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// Sample is one golden-set conversation: the input exactly as the worker would build it (scenario,
// turns, statistics) and its gold labels. Insufficient samples are not scored; they exist to cover
// the 10.4 path.
type Sample struct {
	ID              string
	Level           string
	Insufficient    bool
	StartMode       string
	DurationSeconds int
	Scenario        ScenarioInput
	Turns           []evaluation.TurnRow
	Gold            Gold
}

// sampleEpoch is the session start of every sample; turn timestamps are offsets from it.
var sampleEpoch = time.Date(2026, time.January, 1, 9, 0, 0, 0, time.UTC)

// StartedAt and EndedAt are the session bounds the statistics derive from.
func (s Sample) StartedAt() time.Time { return sampleEpoch }

// EndedAt is StartedAt plus DurationSeconds.
func (s Sample) EndedAt() time.Time {
	return sampleEpoch.Add(time.Duration(s.DurationSeconds) * time.Second)
}

// Stats are the 10.4 statistics of the sample.
func (s Sample) Stats() evaluation.Stats {
	return evaluation.ComputeStats(s.Turns, s.StartedAt(), s.EndedAt(), 0)
}

// EligibleUserTurns counts the user turns the evaluation sees.
func (s Sample) EligibleUserTurns() int { return s.Stats().UserTurns }

type wireTurn struct {
	Seq                 int     `json:"seq"`
	Status              string  `json:"status"`
	UserText            *string `json:"user_text"`
	AssistantText       *string `json:"assistant_text"`
	AssistantSpokenText *string `json:"assistant_spoken_text"`
	UserStartedMS       *int64  `json:"user_started_ms"`
	UserEndedMS         *int64  `json:"user_ended_ms"`
}

type wireSample struct {
	ID              string        `json:"id"`
	Level           string        `json:"level"`
	Insufficient    bool          `json:"insufficient"`
	StartMode       string        `json:"start_mode"`
	DurationSeconds int           `json:"duration_seconds"`
	Scenario        ScenarioInput `json:"scenario"`
	Turns           []wireTurn    `json:"turns"`
	Gold            Gold          `json:"gold"`
}

// UnmarshalJSON reads the JSONL form: turn times are millisecond offsets from the session start
// (user_started_ms/user_ended_ms) and ids and creation times are derived from the sequence.
func (s *Sample) UnmarshalJSON(b []byte) error {
	var w wireSample
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&w); err != nil {
		return err
	}
	*s = Sample{ID: w.ID, Level: w.Level, Insufficient: w.Insufficient, StartMode: w.StartMode,
		DurationSeconds: w.DurationSeconds, Scenario: w.Scenario, Gold: w.Gold}
	for _, t := range w.Turns {
		status := t.Status
		row := evaluation.TurnRow{Seq: t.Seq, Status: &status, UserText: t.UserText, AssistantText: t.AssistantText,
			AssistantSpokenText: t.AssistantSpokenText}
		if t.UserStartedMS != nil {
			v := sampleEpoch.Add(time.Duration(*t.UserStartedMS) * time.Millisecond)
			row.UserStartedAt = &v
			row.CreatedAt = v
		}
		if t.UserEndedMS != nil {
			v := sampleEpoch.Add(time.Duration(*t.UserEndedMS) * time.Millisecond)
			row.UserEndedAt = &v
		}
		s.Turns = append(s.Turns, row)
	}
	return nil
}

// Dataset is a loaded golden-set directory.
type Dataset struct {
	Manifest Manifest
	Samples  []Sample
}

// LoadDataset reads manifest.json and every *.jsonl file (in name order) of dir, one sample per
// line. Unknown fields, duplicate ids, unknown levels/start modes and scored samples without
// complete rater labels are errors.
func LoadDataset(dir string) (Dataset, error) {
	var ds Dataset
	mb, err := os.ReadFile(filepath.Join(dir, "manifest.json")) //nolint:gosec // operator-supplied dataset directory
	if err != nil {
		return ds, fmt.Errorf("golden: read manifest: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(mb))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&ds.Manifest); err != nil {
		return ds, fmt.Errorf("golden: manifest.json: %w", err)
	}
	if ds.Manifest.Status != StatusExample && ds.Manifest.Status != StatusComplete {
		return ds, fmt.Errorf("golden: manifest status %q must be %s or %s", ds.Manifest.Status, StatusExample, StatusComplete)
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil {
		return ds, err
	}
	sort.Strings(files)
	seen := map[string]bool{}
	for _, f := range files {
		samples, err := readJSONL(f)
		if err != nil {
			return ds, err
		}
		for _, s := range samples {
			if err := validateSample(s); err != nil {
				return ds, fmt.Errorf("golden: %s: sample %q: %w", filepath.Base(f), s.ID, err)
			}
			if seen[s.ID] {
				return ds, fmt.Errorf("golden: duplicate sample id %q", s.ID)
			}
			seen[s.ID] = true
			ds.Samples = append(ds.Samples, s)
		}
	}
	if len(ds.Samples) == 0 {
		return ds, errors.New("golden: dataset has no samples")
	}
	return ds, nil
}

func readJSONL(path string) ([]Sample, error) {
	f, err := os.Open(path) //nolint:gosec // operator-supplied dataset directory
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var out []Sample
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	line := 0
	for sc.Scan() {
		line++
		raw := bytes.TrimSpace(sc.Bytes())
		if len(raw) == 0 {
			continue
		}
		var s Sample
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, fmt.Errorf("golden: %s:%d: %w", filepath.Base(path), line, err)
		}
		out = append(out, s)
	}
	return out, sc.Err()
}

func validateSample(s Sample) error {
	if strings.TrimSpace(s.ID) == "" {
		return errors.New("id is empty")
	}
	if !contains(Levels, s.Level) {
		return fmt.Errorf("level %q is not one of %v", s.Level, Levels)
	}
	if !contains(startModes, s.StartMode) {
		return fmt.Errorf("start_mode %q is not one of %v", s.StartMode, startModes)
	}
	insufficient := s.Stats().Insufficient(evaluation.DefaultConfig().Thresholds())
	if s.Insufficient != insufficient {
		return fmt.Errorf("insufficient=%v but its statistics %+v say %v", s.Insufficient, s.Stats(), insufficient)
	}
	if s.Insufficient {
		return nil
	}
	for _, d := range evaluation.LLMDimensions {
		for name, r := range map[string]map[string]int{"rater1": s.Gold.Rater1, "rater2": s.Gold.Rater2} {
			v, ok := r[string(d)]
			if !ok || v < 0 || v > 100 {
				return fmt.Errorf("%s score for %s is missing or outside 0-100", name, d)
			}
		}
		if v, ok := s.Gold.Rater3[string(d)]; ok && (v < 0 || v > 100) {
			return fmt.Errorf("rater3 score for %s is outside 0-100", d)
		}
	}
	for _, m := range s.Gold.MajorErrors {
		if _, ok := evaluation.SpecOf(evaluation.FindingType(m.FindingType)); !ok {
			return fmt.Errorf("major error with unknown finding_type %q", m.FindingType)
		}
	}
	return nil
}

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

func orDefault(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

// CheckComplete lists what keeps the dataset from being the full golden set of spec 10.12: the
// number of scored and insufficient samples, samples per level, scenario kinds per level, both start
// modes, and 6-15 user turns in every scored sample. An empty result means complete.
func (d Dataset) CheckComplete() []string {
	exp := d.Manifest.Expected
	wantSamples := orDefault(exp.Samples, defaultSamples)
	wantPerLevel := orDefault(exp.PerLevel, defaultPerLevel)
	wantInsufficient := orDefault(exp.Insufficient, defaultInsufficient)
	wantKinds := orDefault(exp.MinScenarioKinds, defaultMinScenarioKinds)

	var scored, insufficient int
	perLevel := map[string]int{}
	kinds := map[string]map[string]bool{}
	modes := map[string]bool{}
	var out []string
	for _, s := range d.Samples {
		if s.Insufficient {
			insufficient++
			continue
		}
		scored++
		perLevel[s.Level]++
		if kinds[s.Level] == nil {
			kinds[s.Level] = map[string]bool{}
		}
		kinds[s.Level][s.Scenario.Kind] = true
		modes[s.StartMode] = true
		if n := s.EligibleUserTurns(); n < minUserTurns || n > maxUserTurns {
			out = append(out, fmt.Sprintf("sample %s has %d user turns (want %d-%d)", s.ID, n, minUserTurns, maxUserTurns))
		}
	}
	if scored != wantSamples {
		out = append(out, fmt.Sprintf("%d scored samples (want %d)", scored, wantSamples))
	}
	for _, l := range Levels {
		if perLevel[l] != wantPerLevel {
			out = append(out, fmt.Sprintf("level %s has %d samples (want %d)", l, perLevel[l], wantPerLevel))
		}
		if len(kinds[l]) < wantKinds {
			out = append(out, fmt.Sprintf("level %s covers %d scenario kinds (want at least %d)", l, len(kinds[l]), wantKinds))
		}
	}
	for _, m := range startModes {
		if !modes[m] {
			out = append(out, "no scored sample with start_mode "+m)
		}
	}
	if insufficient != wantInsufficient {
		out = append(out, fmt.Sprintf("%d insufficient-evidence samples (want %d)", insufficient, wantInsufficient))
	}
	return out
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }
