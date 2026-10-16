package evaluation_test

import (
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/evaluation"
)

func ptr[T any](v T) *T { return &v }

var filterTurns = []evaluation.TurnRef{
	{ID: uuid.MustParse("00000000-0000-0000-0000-000000000001"), Seq: 1, UserText: "今日は会社に働いています"},
	{ID: uuid.MustParse("00000000-0000-0000-0000-000000000003"), Seq: 3, UserText: "えっと、昨日は 忙しいでした。"},
	{ID: uuid.MustParse("00000000-0000-0000-0000-000000000005"), Seq: 5, UserText: "ＡＢＣ会社はいい"},
}

// particle is a valid PARTICLE_ERROR finding on turn 1.
func particle() evaluation.RawFinding {
	return evaluation.RawFinding{
		Dimension: "GRAMMAR", FindingType: "PARTICLE_ERROR", Severity: "MAJOR", TurnSeq: ptr(1),
		Original: ptr("会社に働いて"), Corrected: ptr("会社で働いて"),
		Explanation: "Tempat aktivitas ditandai で.", PatternKey: ptr("particle.de_ni"), Confidence: 0.9,
	}
}

func filter(raw ...evaluation.RawFinding) evaluation.FilterResult {
	return evaluation.FilterFindings(raw, filterTurns, 0.6)
}

func TestNormalizeForMatch(t *testing.T) {
	require.Equal(t, "えっと今日は会社", evaluation.NormalizeForMatch("えっと…、今日は 会社"))
	require.Equal(t, "ABC", evaluation.NormalizeForMatch("ＡＢＣ！"))
	require.Equal(t, "会社ですか", evaluation.NormalizeForMatch("「会社」ですか?"))
	require.Equal(t, "ab", evaluation.NormalizeForMatch(" a,b.\t\n・"))
	require.Empty(t, evaluation.NormalizeForMatch("、。，．！？!?「」…・ 　"))
	require.Equal(t, "アイウ", evaluation.NormalizeForMatch("ｱｲｳ"))
}

func TestFilterKeepsValidFindingAndFillsFields(t *testing.T) {
	res := filter(particle())
	require.Len(t, res.Kept, 1)
	require.Empty(t, res.Dropped)
	require.Zero(t, res.Rejected)
	require.Equal(t, 1, res.Total)
	f := res.Kept[0]
	require.Equal(t, evaluation.FindingParticleError, f.Type)
	require.Equal(t, evaluation.DimGrammar, f.Dimension)
	require.Equal(t, evaluation.SeverityMajor, f.Severity)
	require.Equal(t, 1, *f.TurnSeq)
	require.Equal(t, filterTurns[0].ID, *f.TurnID)
	require.Equal(t, "会社に働いて", *f.Original)
	require.Equal(t, "会社で働いて", *f.Corrected)
	require.Equal(t, "particle.de_ni", *f.PatternKey)
	require.Equal(t, "Tempat aktivitas ditandai で.", f.Explanation)
	require.InDelta(t, 0.9, f.Confidence, 1e-9)
	require.Equal(t, "LLM", f.Source)
}

func TestFilterFindingsRules(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(f *evaluation.RawFinding)
		reason   string // "" = kept
		rejected bool
	}{
		{"valid", func(*evaluation.RawFinding) {}, "", false},
		{"type_dimension_mismatch", func(f *evaluation.RawFinding) { f.Dimension = "VOCABULARY" }, evaluation.DropTypeDimensionMismatch, true},
		{"unknown type", func(f *evaluation.RawFinding) { f.FindingType = "NOPE" }, evaluation.DropTypeDimensionMismatch, true},
		{"missing corrected", func(f *evaluation.RawFinding) { f.Corrected = nil }, evaluation.DropMissingField, true},
		{"blank corrected", func(f *evaluation.RawFinding) { f.Corrected = ptr("  ") }, evaluation.DropMissingField, true},
		{"missing original", func(f *evaluation.RawFinding) { f.Original = nil }, evaluation.DropMissingField, true},
		{"missing turn_seq", func(f *evaluation.RawFinding) { f.TurnSeq = nil }, evaluation.DropMissingField, true},
		{"turn_not_found", func(f *evaluation.RawFinding) { f.TurnSeq = ptr(2) }, evaluation.DropTurnNotFound, true},
		{"original_not_found", func(f *evaluation.RawFinding) { f.Original = ptr("学校で働いて") }, evaluation.DropOriginalNotFound, true},
		{"original only punctuation", func(f *evaluation.RawFinding) { f.Original = ptr("、。") }, evaluation.DropOriginalNotFound, true},
		{"original of another turn", func(f *evaluation.RawFinding) { f.Original = ptr("忙しいでした") }, evaluation.DropOriginalNotFound, true},
		{"corrected_unchanged", func(f *evaluation.RawFinding) { f.Corrected = ptr("会社に 働いて。") }, evaluation.DropCorrectedUnchanged, true},
		{"low_confidence 0.59", func(f *evaluation.RawFinding) { f.Confidence = 0.59 }, evaluation.DropLowConfidence, false},
		{"confidence 0.60 kept", func(f *evaluation.RawFinding) { f.Confidence = 0.60 }, "", false},
		{"original matches across whitespace and punctuation", func(f *evaluation.RawFinding) {
			f.TurnSeq, f.Original, f.Corrected = ptr(3), ptr("えっと昨日は忙しいでした"), ptr("昨日は忙しかったです")
		}, "", false},
		{"original matches after NFKC", func(f *evaluation.RawFinding) {
			f.TurnSeq, f.Original, f.Corrected = ptr(5), ptr("ABC会社"), ptr("ABCの会社")
		}, "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := particle()
			tc.mutate(&f)
			res := filter(f)
			require.Equal(t, 1, res.Total)
			if tc.reason == "" {
				require.Len(t, res.Kept, 1)
				require.Empty(t, res.Dropped)
				require.Zero(t, res.Rejected)
				return
			}
			require.Empty(t, res.Kept)
			require.Equal(t, map[string]int{tc.reason: 1}, res.Dropped)
			if tc.rejected {
				require.Equal(t, 1, res.Rejected)
			} else {
				require.Zero(t, res.Rejected)
			}
		})
	}
}

func TestFilterMustBeNullAndSoftTypes(t *testing.T) {
	objective := evaluation.RawFinding{
		Dimension: "TASK_ACHIEVEMENT", FindingType: "OBJECTIVE_NOT_MET", Severity: "MAJOR",
		Explanation: "Tujuan belum tercapai sama sekali.", Confidence: 0.8,
	}
	res := filter(objective)
	require.Len(t, res.Kept, 1)
	require.Nil(t, res.Kept[0].TurnSeq)
	require.Nil(t, res.Kept[0].TurnID)
	require.Nil(t, res.Kept[0].Original)

	for name, mut := range map[string]func(f *evaluation.RawFinding){
		"turn_seq": func(f *evaluation.RawFinding) { f.TurnSeq = ptr(1) },
		"original": func(f *evaluation.RawFinding) { f.Original = ptr("会社") },
		"corrected": func(f *evaluation.RawFinding) {
			f.Corrected = ptr("会社")
		},
	} {
		f := objective
		mut(&f)
		res := filter(f)
		require.Empty(t, res.Kept, name)
		require.Equal(t, map[string]int{evaluation.DropMustBeNull: 1}, res.Dropped, name)
		require.Equal(t, 1, res.Rejected, name)
	}

	// EXCESSIVE_FILLER: corrected optional (nil and a value both pass; unchanged corrected is fine)
	filler := evaluation.RawFinding{
		Dimension: "FLUENCY", FindingType: "EXCESSIVE_FILLER", Severity: "MINOR", TurnSeq: ptr(3), Original: ptr("えっと"),
		Explanation: "Terlalu banyak kata pengisi.", Confidence: 0.8,
	}
	require.Len(t, filter(filler).Kept, 1)
	filler.Corrected = ptr("えっと")
	require.Len(t, filter(filler).Kept, 1, "corrected optional: no corrected_unchanged rule")
}

func TestFilterStrength(t *testing.T) {
	s := evaluation.RawFinding{
		Dimension: "VOCABULARY", FindingType: "STRENGTH", Severity: "MAJOR", TurnSeq: ptr(1), Original: ptr("今日は会社"),
		Corrected: ptr("x"), Explanation: "Pilihan kata sudah tepat.", Confidence: 0.9,
	}
	res := filter(s)
	require.Len(t, res.Kept, 1)
	f := res.Kept[0]
	require.Equal(t, evaluation.SeverityInfo, f.Severity)
	require.Nil(t, f.Corrected)
	require.Equal(t, evaluation.DimVocabulary, f.Dimension)
	require.Equal(t, 1, *f.TurnSeq)

	// turn_seq and original optional
	s2 := s
	s2.TurnSeq, s2.Original = nil, nil
	res = filter(s2)
	require.Len(t, res.Kept, 1)
	require.Nil(t, res.Kept[0].TurnID)

	// a STRENGTH that points at a missing turn or a foreign original is still rejected
	s3 := s
	s3.TurnSeq = ptr(2)
	require.Equal(t, map[string]int{evaluation.DropTurnNotFound: 1}, filter(s3).Dropped)
	s4 := s
	s4.Original = ptr("存在しない")
	require.Equal(t, map[string]int{evaluation.DropOriginalNotFound: 1}, filter(s4).Dropped)
	s5 := s
	s5.TurnSeq = nil // original without a turn cannot be verified
	require.Equal(t, map[string]int{evaluation.DropOriginalNotFound: 1}, filter(s5).Dropped)
	s6 := s
	s6.Dimension = "PRONUNCIATION"
	require.Equal(t, map[string]int{evaluation.DropTypeDimensionMismatch: 1}, filter(s6).Dropped)
}

func TestFilterDuplicates(t *testing.T) {
	a := particle()
	b := particle()
	b.Original, b.Corrected, b.Confidence, b.Severity = ptr("会社に 働いて。"), ptr("会社で働いて"), 0.99, "MINOR"
	res := filter(a, b)
	require.Len(t, res.Kept, 1)
	require.InDelta(t, 0.9, res.Kept[0].Confidence, 1e-9, "the first is kept")
	require.Equal(t, map[string]int{evaluation.DropDuplicate: 1}, res.Dropped)
	require.Zero(t, res.Rejected)

	// same text, different type or turn is not a duplicate
	c := particle()
	c.FindingType, c.Dimension = "CONJUGATION_ERROR", "GRAMMAR"
	d := particle()
	d.TurnSeq, d.Original, d.Corrected = ptr(5), ptr("会社"), ptr("会社の")
	res = filter(a, c, d)
	require.Len(t, res.Kept, 3)

	// a low-confidence first occurrence does not shadow a confident duplicate
	low := particle()
	low.Confidence = 0.1
	res = filter(low, a)
	require.Len(t, res.Kept, 1)
	require.Equal(t, map[string]int{evaluation.DropLowConfidence: 1}, res.Dropped)
}

func TestFilterLimits(t *testing.T) {
	var raw []evaluation.RawFinding
	// 20 distinct errors: originals differ through the turn text; build turns for them.
	var turns []evaluation.TurnRef
	for i := 1; i <= 20; i++ {
		turns = append(turns, evaluation.TurnRef{ID: uuid.New(), Seq: i, UserText: fmt.Sprintf("会社%dで働く", i)})
		f := particle()
		f.TurnSeq = ptr(i)
		f.Original, f.Corrected = ptr(fmt.Sprintf("会社%d", i)), ptr(fmt.Sprintf("会社%dの", i))
		f.Severity = "MINOR"
		f.Confidence = 0.7 + float64(i)/1000 // later = more confident
		if i <= 5 {
			f.Severity = "MAJOR"
			f.Confidence = 0.61 + float64(i)/1000 // MAJOR but the least confident
		}
		raw = append(raw, f)
	}
	// 5 strengths on distinct turns
	for i := 1; i <= 5; i++ {
		s := evaluation.RawFinding{
			Dimension: "FLUENCY", FindingType: "STRENGTH", Severity: "MAJOR", TurnSeq: ptr(i), Original: ptr(fmt.Sprintf("会社%d", i)),
			Explanation: "Lancar sekali.", Confidence: 0.6 + float64(i)/100,
		}
		raw = append(raw, s)
	}
	res := evaluation.FilterFindings(raw, turns, 0.6)
	require.Equal(t, 25, res.Total)
	require.Zero(t, res.Rejected)
	require.Equal(t, map[string]int{evaluation.DropOverLimit: 7}, res.Dropped)
	var errs, strengths int
	seqs := map[int]bool{}
	for _, f := range res.Kept {
		if f.Type == evaluation.FindingStrength {
			strengths++
			seqs[1000+*f.TurnSeq] = true
		} else {
			errs++
			seqs[*f.TurnSeq] = true
		}
	}
	require.Equal(t, 15, errs)
	require.Equal(t, 3, strengths)
	for i := 1; i <= 5; i++ {
		require.True(t, seqs[i], "MAJOR error %d must survive the cut", i)
	}
	for i := 6; i <= 10; i++ {
		require.False(t, seqs[i], "least confident MINOR error %d is cut", i)
	}
	for i := 11; i <= 20; i++ {
		require.True(t, seqs[i], "confident MINOR error %d is kept", i)
	}
	require.True(t, seqs[1005] && seqs[1004] && seqs[1003], "the 3 most confident strengths")
	require.False(t, seqs[1002] || seqs[1001])

	// 15 errors + 3 strengths exactly: nothing is dropped
	res = evaluation.FilterFindings(append(raw[5:20:20], raw[22:25]...), turns, 0.6)
	require.Len(t, res.Kept, 18)
	require.Empty(t, res.Dropped)
}

func TestFilterKeepsInputOrder(t *testing.T) {
	a := particle()
	b := particle()
	b.TurnSeq, b.Original, b.Corrected, b.Severity = ptr(3), ptr("昨日は忙しい"), ptr("昨日は忙しかった"), "INFO"
	c := particle()
	c.TurnSeq, c.Original, c.Corrected, c.Severity = ptr(5), ptr("会社はいい"), ptr("会社はよい"), "MINOR"
	res := filter(b, c, a)
	require.Len(t, res.Kept, 3)
	require.Equal(t, []int{3, 5, 1}, []int{*res.Kept[0].TurnSeq, *res.Kept[1].TurnSeq, *res.Kept[2].TurnSeq})
}

func TestTooManyRejected(t *testing.T) {
	bad := func() evaluation.RawFinding { f := particle(); f.TurnSeq = ptr(99); return f }
	good := func(i int) evaluation.RawFinding {
		f := particle()
		f.TurnSeq, f.Original, f.Corrected = ptr(3), ptr([]string{"昨日は", "忙しい", "えっと", "でした"}[i]), ptr(fmt.Sprintf("x%d", i))
		return f
	}

	res := filter(bad(), bad(), bad(), good(0), good(1))
	require.Equal(t, 5, res.Total)
	require.Equal(t, 3, res.Rejected)
	require.True(t, res.TooManyRejected())

	res = filter(bad(), bad(), good(0), good(1), good(2))
	require.Equal(t, 2, res.Rejected)
	require.False(t, res.TooManyRejected())

	res = filter(bad(), good(0))
	require.Equal(t, 1, res.Rejected)
	require.False(t, res.TooManyRejected(), "exactly 50 percent is not more than half")

	res = filter(bad())
	require.True(t, res.TooManyRejected())

	require.False(t, filter().TooManyRejected(), "no findings is fine")

	// low confidence, duplicates and over-limit never count as rejected
	low := good(0)
	low.Confidence = 0.1
	res = filter(low, low, low, good(1))
	require.Equal(t, 3, res.Dropped[evaluation.DropLowConfidence])
	require.Zero(t, res.Rejected)
	require.False(t, res.TooManyRejected())
	dup := good(2)
	res = filter(dup, dup, dup)
	require.Equal(t, 2, res.Dropped[evaluation.DropDuplicate])
	require.False(t, res.TooManyRejected())
}

func TestFilterDuplicateTurnRefFirstWins(t *testing.T) {
	first := uuid.New()
	turns := []evaluation.TurnRef{
		{ID: first, Seq: 1, UserText: "今日は会社に働いています"},
		{ID: uuid.New(), Seq: 1, UserText: "別の文章です"},
	}
	res := evaluation.FilterFindings([]evaluation.RawFinding{particle()}, turns, 0.6)
	require.Len(t, res.Kept, 1)
	require.Equal(t, first, *res.Kept[0].TurnID)
}
