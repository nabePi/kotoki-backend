package learner_test

import (
	"math/rand"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/learner"
)

func goldenEstimates() []learner.Estimate {
	t0 := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	return []learner.Estimate{
		{EvaluationID: uuid.MustParse("00000000-0000-0000-0000-000000000001"), AssessedAt: t0, RawScore: 82, DEff: intp(2), LObs: 2.40, CObs: 0.800},
		{EvaluationID: uuid.MustParse("00000000-0000-0000-0000-000000000002"), AssessedAt: t0.AddDate(0, 0, 3), RawScore: 91, DEff: intp(2), LObs: 2.70, CObs: 0.638},
		{EvaluationID: uuid.MustParse("00000000-0000-0000-0000-000000000003"), AssessedAt: t0.AddDate(0, 0, 7), RawScore: 58, DEff: intp(3), LObs: 2.66, CObs: 0.800},
	}
}

var n4Prior = learner.SkillState{Level: 2.00, Confidence: 0.100}

func permutations(in []learner.Estimate) [][]learner.Estimate {
	var out [][]learner.Estimate
	var rec func(k int, a []learner.Estimate)
	rec = func(k int, a []learner.Estimate) {
		if k == len(a) {
			out = append(out, append([]learner.Estimate(nil), a...))
			return
		}
		for i := k; i < len(a); i++ {
			a[k], a[i] = a[i], a[k]
			rec(k+1, a)
			a[k], a[i] = a[i], a[k]
		}
	}
	rec(0, append([]learner.Estimate(nil), in...))
	return out
}

func TestReplay_OutOfOrderEqualsInOrder(t *testing.T) {
	ests := goldenEstimates()
	want, wantRows := learner.Replay(n4Prior, ests)
	require.Equal(t, 2.35, want.Level)
	require.Equal(t, 0.502, want.Confidence)
	require.Equal(t, 3, want.Count)
	require.Equal(t, ests[2].AssessedAt, *want.LastEvidenceAt)
	require.Equal(t, 2.35, *want.Ceiling)
	require.Len(t, wantRows, 3)
	require.Equal(t, learner.TrendImproving, wantRows[2].Trend)
	require.Equal(t, learner.TrendInsufficient, wantRows[1].Trend)
	require.Equal(t, 2.27, wantRows[2].Previous)

	perms := permutations(ests)
	require.Len(t, perms, 6)
	for _, p := range perms {
		got, rows := learner.Replay(n4Prior, p)
		require.Equal(t, want, got)
		require.Equal(t, wantRows, rows)
	}
}

func TestReplayIdempotent(t *testing.T) {
	ests := goldenEstimates()
	a, ra := learner.Replay(n4Prior, ests)
	b, rb := learner.Replay(n4Prior, ests)
	require.Equal(t, a, b)
	require.Equal(t, ra, rb)
}

func TestReplayDoesNotMutateInput(t *testing.T) {
	ests := goldenEstimates()
	ests[0], ests[2] = ests[2], ests[0]
	first := ests[0].EvaluationID
	learner.Replay(n4Prior, ests)
	require.Equal(t, first, ests[0].EvaluationID)
}

func TestReplayEmpty(t *testing.T) {
	prior := learner.SkillState{Level: 3.0, Confidence: 0.1}
	got, rows := learner.Replay(prior, nil)
	require.Equal(t, prior, got)
	require.Empty(t, rows)
}

func TestReplayTieBreakByEvaluationID(t *testing.T) {
	at := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	lo := learner.Estimate{EvaluationID: uuid.MustParse("00000000-0000-0000-0000-00000000000a"), AssessedAt: at, LObs: 4.0, CObs: 1.0}
	hi := learner.Estimate{EvaluationID: uuid.MustParse("00000000-0000-0000-0000-00000000000b"), AssessedAt: at, LObs: 1.0, CObs: 1.0}
	f1, r1 := learner.Replay(n4Prior, []learner.Estimate{hi, lo})
	f2, r2 := learner.Replay(n4Prior, []learner.Estimate{lo, hi})
	require.Equal(t, f1, f2)
	require.Equal(t, r1, r2)
	// lo (UUID ...0a) is applied first: 2.00 -> 2.40 (rise cap), then hi: 2.40 -> drop.
	require.Equal(t, 2.40, r1[0].Level)
	require.Less(t, r1[1].Level, 2.40)
}

func TestStepEqualsReplayAppend(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	t0 := time.Date(2026, 6, 1, 8, 0, 0, 0, time.UTC)
	var ests []learner.Estimate
	for i := 0; i < 5; i++ {
		d := 1 + rng.Intn(5)
		score := rng.Intn(101)
		ests = append(ests, learner.Estimate{
			EvaluationID: uuid.New(),
			AssessedAt:   t0.AddDate(0, 0, i*(1+rng.Intn(20))+i),
			RawScore:     score,
			DEff:         intp(d),
			LObs:         learner.EvidenceLevel(learner.SkillGrammar, score, d),
			CObs:         learner.EvidenceConfidence(0.5+rng.Float64()/2, 0.5+rng.Float64()/2),
		})
	}
	sort.SliceStable(ests, func(i, j int) bool { return ests[i].AssessedAt.Before(ests[j].AssessedAt) })

	st := n4Prior
	var hist []learner.TrendPoint
	var rows []learner.Applied
	for _, e := range ests {
		var a learner.Applied
		st, a = learner.Step(st, e, hist)
		rows = append(rows, a)
		hist = append(hist, learner.TrendPoint{At: e.AssessedAt, Level: a.Level})
	}
	final, replayRows := learner.Replay(n4Prior, ests)
	require.Equal(t, st, final)
	require.Equal(t, rows, replayRows)
}
