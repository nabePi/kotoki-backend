//go:build !windows

package scenario

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/kotokiclient"
)

// fixtures mirrors testdata/audio/synth/manifest.json.
func fixtures() []Fixture {
	return []Fixture{
		{Name: "ja_greeting_short", DurationMS: 1500, Tags: []string{"speech", "e1", "load"}},
		{Name: "ja_self_intro", DurationMS: 4000, Tags: []string{"speech", "e1", "live-stt"}},
		{Name: "ja_filler_pause", DurationMS: 4000, Tags: []string{"speech", "turn-engine", "filler", "pause"}},
		{Name: "ja_conjunction_pause", DurationMS: 4000, Tags: []string{"speech", "turn-engine", "conjunction", "pause"}},
		{Name: "ja_question", DurationMS: 2000, Tags: []string{"speech", "turn-engine", "question"}},
		{Name: "ja_long_utterance", DurationMS: 20000, Tags: []string{"speech", "load", "stt", "long"}},
		{Name: "ja_max_utterance", DurationMS: 50000, Tags: []string{"speech", "turn-engine", "max-utterance", "long"}},
		{Name: "ja_barge_in", DurationMS: 3000, Tags: []string{"speech", "e3", "load", "barge-in"}},
		{Name: "noise_only", DurationMS: 5000, Tags: []string{"noise", "vad"}},
		{Name: "silence_5s", DurationMS: 5000, Tags: []string{"silence", "pacing"}},
	}
}

func durOf(name string) time.Duration {
	for _, f := range fixtures() {
		if f.Name == name {
			return time.Duration(f.DurationMS) * time.Millisecond
		}
	}
	return 0
}

func tagsOf(name string) []string {
	for _, f := range fixtures() {
		if f.Name == name {
			return f.Tags
		}
	}
	return nil
}

func TestPlanner_Deterministic(t *testing.T) {
	a, b := NewPlanner(42, fixtures()), NewPlanner(42, fixtures())
	for i := 0; i < 100; i++ {
		require.Equal(t, a.Next(), b.Next(), "plan %d", i)
	}
	c := NewPlanner(43, fixtures())
	d := NewPlanner(42, fixtures())
	same := true
	for i := 0; i < 20 && same; i++ {
		same = len(c.Next().Turns) == len(d.Next().Turns)
	}
	require.False(t, same, "another seed must give another sequence")
}

func TestPlanner_ForVU_SeedsPerVU(t *testing.T) {
	base := NewPlanner(7, fixtures())
	require.Equal(t, base.ForVU(3).Next(), NewPlanner(10, fixtures()).Next())
	require.NotEqual(t, base.ForVU(0).Next(), base.ForVU(1).Next())
}

func pct(n, total int) float64 { return 100 * float64(n) / float64(total) }

func TestPlanner_Distributions(t *testing.T) {
	const plans = 20000
	p := NewPlanner(1, fixtures())
	var d300, d600, d900, aiFirst, drops, waitExpiry int
	var turns, turnsAfterFirst, barge, filler int
	for i := 0; i < plans; i++ {
		s := p.Next()
		switch s.DurationSeconds {
		case 300:
			d300++
		case 600:
			d600++
		case 900:
			d900++
		default:
			t.Fatalf("duration %d", s.DurationSeconds)
		}
		switch s.StartMode {
		case "AI_FIRST":
			aiFirst++
		case "USER_FIRST":
		default:
			t.Fatalf("start mode %q", s.StartMode)
		}
		if s.Drop != nil {
			drops++
			require.GreaterOrEqual(t, s.Drop.For, time.Second)
			require.LessOrEqual(t, s.Drop.For, 5*time.Second)
			require.Less(t, s.Drop.AfterTurn, len(s.Turns))
		}
		if s.EndAfter == 0 {
			waitExpiry++
		} else {
			require.Less(t, s.EndAfter, time.Duration(s.DurationSeconds)*time.Second)
		}
		require.NotEmpty(t, s.Turns)
		for j, tp := range s.Turns {
			turns++
			require.GreaterOrEqual(t, tp.Think, time.Second)
			require.LessOrEqual(t, tp.Think, 3*time.Second)
			if j > 0 {
				turnsAfterFirst++
			}
			if tp.BargeInAfter != 0 {
				require.Positive(t, j, "turn 0 has nothing to interrupt")
				require.GreaterOrEqual(t, tp.BargeInAfter, time.Second)
				require.LessOrEqual(t, tp.BargeInAfter, 2*time.Second)
				require.Equal(t, "ja_barge_in", tp.Fixture)
				barge++
				continue
			}
			tags := tagsOf(tp.Fixture)
			require.NotNil(t, tags, tp.Fixture)
			if tp.Filler {
				filler++
				require.Contains(t, tags, "filler")
				continue
			}
			for _, bad := range []string{"noise", "silence", "filler"} {
				require.NotContains(t, tags, bad, tp.Fixture)
			}
		}
	}
	require.InDelta(t, 60, pct(d300, plans), 1.5)
	require.InDelta(t, 30, pct(d600, plans), 1.5)
	require.InDelta(t, 10, pct(d900, plans), 1.0)
	require.InDelta(t, 50, pct(aiFirst, plans), 1.5)
	require.InDelta(t, 2, pct(drops, plans), 0.5)
	require.InDelta(t, 70, pct(waitExpiry, plans), 1.5)
	require.InDelta(t, 10, pct(barge, turnsAfterFirst), 1.0, "barge-in rate per eligible turn (turn 0 cannot barge in)")
	require.InDelta(t, 15, pct(filler, turns), 1.0)
}

func TestPlanner_SpeechFixturesAre2To8Seconds(t *testing.T) {
	p := NewPlanner(5, fixtures())
	seen := map[string]bool{}
	for i := 0; i < 500; i++ {
		for _, tp := range p.Next().Turns {
			if tp.BargeInAfter == 0 && !tp.Filler {
				seen[tp.Fixture] = true
			}
		}
	}
	require.Equal(t, map[string]bool{"ja_self_intro": true, "ja_conjunction_pause": true, "ja_question": true}, seen)
}

func TestPlanner_TurnsFillTheSession(t *testing.T) {
	p := NewPlanner(9, fixtures())
	for i := 0; i < 200; i++ {
		s := p.Next()
		target := time.Duration(s.DurationSeconds) * time.Second
		if s.EndAfter > 0 {
			target = s.EndAfter
		}
		var est, last time.Duration
		for _, tp := range s.Turns {
			last = durOf(tp.Fixture) + tp.Think + estimatedTurn
			est += last
		}
		require.GreaterOrEqual(t, est, target, "enough turns to keep the VU busy until %s", target)
		require.Less(t, est-last, target, "no turn more than needed")
	}
}

func TestPlanner_NoUsableFixtures(t *testing.T) {
	p := NewPlanner(1, []Fixture{{Name: "noise_only", DurationMS: 5000, Tags: []string{"noise"}}})
	require.Empty(t, p.Next().Turns, "no speech fixture: no turns (the CLI refuses to start before this)")
	require.Empty(t, p.FixtureNames())
}

func TestFixtureNames(t *testing.T) {
	names := NewPlanner(1, fixtures()).FixtureNames()
	require.True(t, slices.IsSorted(names))
	require.Equal(t, []string{"ja_barge_in", "ja_conjunction_pause", "ja_filler_pause", "ja_question", "ja_self_intro"}, names)
}

func TestLoadFixtures(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "synth"), 0o750))
	man := `{"fixtures":[{"name":"ja_question","duration_ms":2000,"tags":["speech"]}]}`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "synth", "manifest.json"), []byte(man), 0o600))

	fx, resolved, err := LoadFixtures(dir) // falls back to <dir>/synth
	require.NoError(t, err)
	require.Equal(t, filepath.Join(dir, "synth"), resolved)
	require.Equal(t, []Fixture{{Name: "ja_question", DurationMS: 2000, Tags: []string{"speech"}}}, fx)

	_, _, err = LoadFixtures(t.TempDir())
	require.Error(t, err)
}

func TestLoadFixtures_RealManifest(t *testing.T) {
	fx, _, err := LoadFixtures(filepath.Join("..", "..", "..", "testdata", "audio"))
	require.NoError(t, err)
	require.Equal(t, fixtures(), fx)
}

func TestSteps_ShapeMatchesPlan(t *testing.T) {
	pre := map[string][][]byte{
		"ja_self_intro": {{1}}, "ja_question": {{2}}, "ja_barge_in": {{3}}, "ja_filler_pause": {{4}},
	}
	plan := SessionPlan{
		DurationSeconds: 300, StartMode: "AI_FIRST", EndAfter: 90 * time.Second,
		Turns: []TurnPlan{
			{Fixture: "ja_self_intro", Think: 2 * time.Second},
			{Fixture: "ja_question", Think: 1 * time.Second, BargeInAfter: 1500 * time.Millisecond}, // the barge-in speaks ja_barge_in whatever Fixture says
			{Fixture: "ja_filler_pause", Think: 3 * time.Second, Filler: true},
			{Fixture: "ja_question", Think: time.Second},
		},
		Drop: &DropPlan{AfterTurn: 2, For: 4 * time.Second},
	}
	want := []kotokiclient.Step{
		kotokiclient.WaitAIDone{},
		kotokiclient.Think{D: 2 * time.Second},
		kotokiclient.Speak{Packets: pre["ja_self_intro"], Label: "ja_self_intro"},
		kotokiclient.BargeIn{After: 1500 * time.Millisecond, Packets: pre["ja_barge_in"]},
		kotokiclient.WaitAIDone{},
		kotokiclient.Think{D: 3 * time.Second},
		kotokiclient.Speak{Packets: pre["ja_filler_pause"], Label: "ja_filler_pause"},
		kotokiclient.Drop{For: 4 * time.Second},
		kotokiclient.WaitAIDone{},
		kotokiclient.Think{D: time.Second},
		kotokiclient.Speak{Packets: pre["ja_question"], Label: "ja_question"},
		kotokiclient.WaitAIDone{},
		kotokiclient.End{},
	}
	require.Equal(t, want, plan.Steps(pre))
}

func TestSteps_UserFirstWaitExpiry(t *testing.T) {
	pre := map[string][][]byte{"ja_question": {{2}}}
	plan := SessionPlan{DurationSeconds: 300, StartMode: "USER_FIRST", Turns: []TurnPlan{{Fixture: "ja_question", Think: time.Second}}}
	want := []kotokiclient.Step{
		kotokiclient.Speak{Packets: pre["ja_question"], Label: "ja_question"},
		kotokiclient.WaitAIDone{},
		kotokiclient.Think{D: 300*time.Second + ExpiryMargin},
	}
	require.Equal(t, want, plan.Steps(pre))
}

func TestSteps_FirstTurnBargeInDegradesToSpeak(t *testing.T) {
	pre := map[string][][]byte{"ja_barge_in": {{3}}}
	plan := SessionPlan{StartMode: "USER_FIRST", Turns: []TurnPlan{{Fixture: "ja_barge_in", BargeInAfter: time.Second}}}
	require.IsType(t, kotokiclient.Speak{}, plan.Steps(pre)[0])
}
