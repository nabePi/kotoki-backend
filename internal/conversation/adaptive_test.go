package conversation

import (
	"reflect"
	"testing"

	"github.com/nabePi/kotoki-backend/internal/learner"
)

func adaptiveFixture() AdaptiveState {
	return AdaptiveState{Current: learner.AdaptiveDifficulty{
		SpeechSpeed: 3, TTSSpeakingRate: 0.95, MaxAISentencesPerTurn: 3,
		QuestionComplexity: 3, Scaffolding: 1, FocusSkills: []learner.Skill{learner.Skill("GRAMMAR")},
	}, TurnsSinceAdjust: 3}
}

var sixTexts = []string{"こんにちは", "もう一度お願いします", "はい", "ゆっくり話してください", "そうです", "好きです"}

func TestMaybeAdjust(t *testing.T) {
	t.Run("adjusts", func(t *testing.T) {
		in := adaptiveFixture()
		snap := adaptiveFixture()
		out, ok := MaybeAdjust(in, sixTexts)
		if !ok {
			t.Fatal("want adjustment")
		}
		c := out.Current
		if c.SpeechSpeed != 2 || c.TTSSpeakingRate != learner.TTSSpeakingRate(2) || c.MaxAISentencesPerTurn != learner.MaxAISentences(2) ||
			c.QuestionComplexity != 2 || c.Scaffolding != 2 || c.InSessionAdjustments != 1 || out.TurnsSinceAdjust != 0 {
			t.Fatalf("bad state %+v turns=%d", c, out.TurnsSinceAdjust)
		}
		if !reflect.DeepEqual(in, snap) {
			t.Fatalf("input mutated: %+v", in)
		}
	})
	t.Run("clamps", func(t *testing.T) {
		in := adaptiveFixture()
		in.Current.SpeechSpeed, in.Current.QuestionComplexity, in.Current.Scaffolding = 1, 1, 3
		out, ok := MaybeAdjust(in, sixTexts)
		if !ok || out.Current.SpeechSpeed != 1 || out.Current.QuestionComplexity != 1 || out.Current.Scaffolding != 3 {
			t.Fatalf("clamp failed: %v %+v", ok, out.Current)
		}
	})
	t.Run("one signal", func(t *testing.T) {
		out, ok := MaybeAdjust(adaptiveFixture(), []string{"もう一度", "はい", "いいえ"})
		if ok || out.TurnsSinceAdjust != 4 || out.Current.InSessionAdjustments != 0 {
			t.Fatalf("got %v %+v", ok, out)
		}
	})
	t.Run("too soon", func(t *testing.T) {
		in := adaptiveFixture()
		in.TurnsSinceAdjust = 2 // becomes 3
		out, ok := MaybeAdjust(in, sixTexts)
		if ok || out.TurnsSinceAdjust != 3 {
			t.Fatalf("got %v %d", ok, out.TurnsSinceAdjust)
		}
	})
	t.Run("exactly gap", func(t *testing.T) {
		in := adaptiveFixture()
		in.TurnsSinceAdjust = 3 // becomes 4
		if _, ok := MaybeAdjust(in, sixTexts); !ok {
			t.Fatal("4 turns since adjust must adjust")
		}
	})
	t.Run("already twice", func(t *testing.T) {
		in := adaptiveFixture()
		in.Current.InSessionAdjustments = 2
		in.TurnsSinceAdjust = 10
		if out, ok := MaybeAdjust(in, sixTexts); ok || out.Current.SpeechSpeed != 3 {
			t.Fatalf("got %v %+v", ok, out.Current)
		}
	})
	t.Run("only last six count", func(t *testing.T) {
		texts := append([]string{"もう一度", "ゆっくり"}, "a", "b", "c", "d", "e", "f")
		if _, ok := MaybeAdjust(adaptiveFixture(), texts); ok {
			t.Fatal("signals older than 6 turns must not count")
		}
	})
}

func TestClarificationRegexAlternatives(t *testing.T) {
	for _, s := range []string{"もう一度", "もう一回", "ゆっくり", "わかりません", "分かりません", "わからない", "分からない",
		"聞き取れ", "何て言いました", "なんて言いました", "どういう意味"} {
		if !ClarificationRegex.MatchString("あの、" + s + "か") {
			t.Errorf("%q not matched", s)
		}
	}
	if ClarificationRegex.MatchString("こんにちは") {
		t.Error("false positive")
	}
}

func TestDifficultyMetricNilSafe(*testing.T) {
	var m *Metrics
	m.difficultyAdjustedInc()
	NewMetrics(nil).difficultyAdjustedInc()
}
