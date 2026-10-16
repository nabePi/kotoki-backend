package golden

import (
	"context"
	"fmt"
	"time"

	"github.com/nabePi/kotoki-backend/internal/evaluation"
)

// completeDataset builds a synthetic COMPLETE set: 80 scored samples (20 per level, four scenario
// kinds, both start modes, six user turns) and 8 insufficient ones. Gold scores rise with the level
// and vary inside it.
func completeDataset() Dataset {
	var ds Dataset
	ds.Manifest.RubricVersion = "rubric-v1"
	ds.Manifest.Status = StatusComplete
	kinds := []string{"daily", "job_interview", "culture", "free_talk"}
	modes := []string{"USER_FIRST", "AI_FIRST"}
	for li, level := range Levels {
		for i := range 20 {
			base := 30 + 20*li
			g := func(shift int) map[string]int {
				return map[string]int{"GRAMMAR": base + i%10 + shift, "VOCABULARY": base + (i+3)%10 + shift,
					"FLUENCY": base + (i+5)%10 + shift, "NATURALNESS": base + (i+7)%10 + shift, "TASK_ACHIEVEMENT": base + (i+1)%10 + shift}
			}
			s := Sample{ID: fmt.Sprintf("%s-%d", level, i), Level: level, StartMode: modes[i%2], DurationSeconds: 200,
				Scenario: ScenarioInput{Kind: kinds[i%4]}, Gold: Gold{Rater1: g(0), Rater2: g(2)}}
			s.Turns = turns(6)
			if i%4 == 0 {
				s.Gold.MajorErrors = []MajorError{{TurnSeq: 2, Original: "x", FindingType: "PARTICLE_ERROR"}}
			}
			ds.Samples = append(ds.Samples, s)
		}
	}
	for i := range 8 {
		ds.Samples = append(ds.Samples, Sample{ID: fmt.Sprintf("ins-%d", i), Level: "BEGINNER", Insufficient: true,
			StartMode: "USER_FIRST", DurationSeconds: 30, Turns: turns(2), Scenario: ScenarioInput{Kind: "daily"}})
	}
	return ds
}

// turns returns n completed turns of 7 s of user speech each, 18 s apart.
func turns(n int) []evaluation.TurnRow {
	out := make([]evaluation.TurnRow, 0, n)
	for i := range n {
		status, text := "COMPLETED", "テストです。"
		start := sampleEpoch.Add(time.Duration(i*18) * time.Second)
		end := start.Add(7 * time.Second)
		out = append(out, evaluation.TurnRow{Seq: i + 1, Status: &status, UserText: &text, UserStartedAt: &start, UserEndedAt: &end})
	}
	return out
}

func setKind(d *Dataset, level, kind string) {
	for i := range d.Samples {
		if d.Samples[i].Level == level {
			d.Samples[i].Scenario.Kind = kind
		}
	}
}

func setMode(d *Dataset, mode string) {
	for i := range d.Samples {
		d.Samples[i].StartMode = mode
	}
}

// perfect answers floor(gold) for every dimension, one finding per gold MAJOR error, and a valid
// first call; tweak mutates the result per sample and run.
func perfect(tweak func(s Sample, run int, r *RunResult)) Evaluator {
	return func(_ context.Context, s Sample, run int) (RunResult, error) {
		r := RunResult{Scores: map[string]int{}, FirstCallValid: true}
		for _, d := range evaluation.LLMDimensions {
			g, err := s.Gold.Score(string(d))
			if err != nil {
				return r, err
			}
			r.Scores[string(d)] = int(g)
		}
		for _, m := range s.Gold.MajorErrors {
			r.Findings = append(r.Findings, FindingKey{TurnSeq: m.TurnSeq, Type: m.FindingType,
				Dimension: string(evaluation.FindingDimension[evaluation.FindingType(m.FindingType)]), Severity: "MAJOR", Original: m.Original})
		}
		if tweak != nil {
			tweak(s, run, &r)
		}
		return r, nil
	}
}
