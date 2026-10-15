package audiofx_test

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nabePi/kotoki-backend/internal/conversation/audio"
	"github.com/nabePi/kotoki-backend/internal/testutil/audiofx"
)

// wantDurations is the §14.5 fixture table (ms). The two pause fixtures are
// 4 s: speech + the mandated pause (checked separately) + speech.
var wantDurations = map[string]int{
	"ja_greeting_short":    1500,
	"ja_self_intro":        4000,
	"ja_filler_pause":      4000,
	"ja_conjunction_pause": 4000,
	"ja_question":          2000,
	"ja_long_utterance":    20000,
	"ja_max_utterance":     50000,
	"ja_barge_in":          3000,
	"noise_only":           5000,
	"silence_5s":           5000,
}

// wantPauses is the gap (ms) between the first and second speech segment.
var wantPauses = map[string]int{
	"ja_filler_pause":      1000,
	"ja_conjunction_pause": 1100,
}

const (
	durationTolMs  = 20
	maxSynthBytes  = 10_000_000 // spec says < 8 MB; infeasible for 98.5 s of 48 kHz PCM16 (deviation)
	frameMs        = 20
	minVoicedRatio = 0.60
	maxNoiseRatio  = 0.05
	minPauseFrames = 40
)

func TestAudioFixtures(t *testing.T) {
	entries := audiofx.FixtureManifest(t)
	seen := map[string]bool{}
	for _, e := range entries {
		seen[e.Name] = true
	}
	for name := range wantDurations {
		if !seen[name] {
			t.Errorf("manifest misses fixture %q", name)
		}
	}
	for _, e := range entries {
		t.Run(e.Name, func(t *testing.T) {
			want, ok := wantDurations[e.Name]
			if !ok {
				t.Fatalf("unexpected fixture %q in manifest", e.Name)
			}
			if e.DurationMs != want {
				t.Fatalf("manifest duration_ms = %d, want %d", e.DurationMs, want)
			}
			if len(e.Tags) == 0 || e.Expect == nil {
				t.Fatalf("manifest entry needs tags and expect: %+v", e)
			}
			checkCanonicalHeader(t, audiofx.Path(t, e.Name, ".wav"))
			pcm, transcript := audiofx.LoadFixture(t, e.Name)
			if pcm.Rate != 48000 {
				t.Fatalf("rate = %d, want 48000", pcm.Rate)
			}
			gotMs := len(pcm.Samples) * 1000 / pcm.Rate
			if d := gotMs - e.DurationMs; d < -durationTolMs || d > durationTolMs {
				t.Fatalf("wav duration %d ms, manifest %d ms (±%d)", gotMs, e.DurationMs, durationTolMs)
			}
			raw, err := os.ReadFile(audiofx.Path(t, e.Name, ".txt"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Count(string(raw), "\n") != 1 || !strings.HasSuffix(string(raw), "\n") || strings.TrimSpace(transcript) == "" {
				t.Fatalf(".txt must hold exactly one non-empty line, got %q", raw)
			}
			checkSegments(t, e, pcm)
			checkPartials(t, e)
		})
	}
}

// checkCanonicalHeader enforces "no extra metadata chunk": fmt at 12, data at 36.
func checkCanonicalHeader(t *testing.T, path string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) < 44 || string(b[12:16]) != "fmt " || string(b[36:40]) != "data" ||
		int(binary.LittleEndian.Uint32(b[40:])) != len(b)-44 {
		t.Fatalf("%s: not a canonical 44-byte-header WAV", filepath.Base(path))
	}
}

// checkSegments verifies speech segment bounds and that everything outside a
// segment is digital zero (except noise_only, which has no segments).
func checkSegments(t *testing.T, e audiofx.Entry, pcm audio.PCM) {
	t.Helper()
	speech := e.Expect.Turns > 0
	if speech != (len(e.Segments) > 0) {
		t.Fatalf("speech fixture must list segments and vice versa (turns=%d, segments=%d)", e.Expect.Turns, len(e.Segments))
	}
	prevEnd := 0
	for _, s := range e.Segments {
		if s.StartMs < prevEnd || s.EndMs <= s.StartMs || s.EndMs > e.DurationMs {
			t.Fatalf("bad segment %+v", s)
		}
		prevEnd = s.EndMs
	}
	if want, ok := wantPauses[e.Name]; ok {
		if len(e.Segments) != 2 || e.Segments[1].StartMs-e.Segments[0].EndMs != want {
			t.Fatalf("want exactly 2 segments separated by %d ms, got %+v", want, e.Segments)
		}
	}
	if e.Name == "noise_only" {
		return
	}
	perMs := pcm.Rate / 1000
	for i, v := range pcm.Samples {
		if v != 0 && !e.InSegment(i/perMs) {
			t.Fatalf("sample %d (%d ms) outside speech is %d, want digital zero", i, i/perMs, v)
		}
	}
}

func checkPartials(t *testing.T, e audiofx.Entry) {
	t.Helper()
	parts := audiofx.LoadPartials(t, e.Name)
	if e.Expect.Turns == 0 {
		if len(parts) != 0 {
			t.Fatalf("non-speech fixture must not have partials")
		}
		return
	}
	if len(parts) == 0 {
		t.Fatalf("speech fixture needs a .partials.json timeline")
	}
	prev := 0
	for i, p := range parts {
		if p.AtMs <= prev || p.AtMs > e.DurationMs || p.Text == "" {
			t.Fatalf("partial %d %+v: at_ms must increase within the fixture and text must be set", i, p)
		}
		if p.Final && i != len(parts)-1 {
			t.Fatalf("only the last partial may be final")
		}
		prev = p.AtMs
	}
}

// TestFixturePartialRoles pins the transcript roles the turn-engine component
// tests (Task 9) depend on.
func TestFixturePartialRoles(t *testing.T) {
	textAt := func(parts []audiofx.Partial, ms int) string {
		s := ""
		for _, p := range parts {
			if p.AtMs <= ms {
				s = p.Text
			}
		}
		return s
	}
	seg := func(name string) []audiofx.Segment {
		for _, e := range audiofx.FixtureManifest(t) {
			if e.Name == name {
				return e.Segments
			}
		}
		t.Fatalf("no manifest entry %q", name)
		return nil
	}

	fp := audiofx.LoadPartials(t, "ja_filler_pause")
	if got := textAt(fp, seg("ja_filler_pause")[1].StartMs); got != "えっと" {
		t.Errorf("ja_filler_pause: text during the pause = %q, want filler-only えっと", got)
	}
	cp := audiofx.LoadPartials(t, "ja_conjunction_pause")
	if got := textAt(cp, seg("ja_conjunction_pause")[1].StartMs); !strings.HasSuffix(got, "けど") {
		t.Errorf("ja_conjunction_pause: text during the pause = %q, want it to end in けど", got)
	}
	q := audiofx.LoadPartials(t, "ja_question")
	if last := q[len(q)-1]; !last.Final || !strings.HasSuffix(strings.TrimRight(last.Text, "？?"), "か") {
		t.Errorf("ja_question: last partial %+v must be final and end in か", last)
	}
	for _, p := range audiofx.LoadPartials(t, "ja_greeting_short") {
		if p.Final {
			t.Errorf("ja_greeting_short must not carry a final (default 800 ms threshold)")
		}
	}
	b := audiofx.LoadPartials(t, "ja_barge_in")
	onset := seg("ja_barge_in")[0].StartMs
	if got := textAt(b, onset+700); len([]rune(got)) < 2 {
		t.Errorf("ja_barge_in: partial at onset+700 ms = %q, want ≥ 2 runes", got)
	}
}

func TestSynthDirSize(t *testing.T) {
	var total int64
	err := filepath.Walk(audiofx.SynthDir(t), func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			total += info.Size()
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if total > maxSynthBytes {
		t.Fatalf("testdata/audio/synth is %d bytes, limit %d", total, maxSynthBytes)
	}
}

// vadFlags runs the realtime VAD (mode 2) over Decimate48to16(pcm) in 20 ms frames.
func vadFlags(t *testing.T, pcm audio.PCM) []bool {
	t.Helper()
	v, err := audio.NewVAD(2)
	if err != nil {
		t.Fatal(err)
	}
	s16 := audio.Decimate48to16(pcm.Samples)
	flags := make([]bool, 0, len(s16)/audio.UpstreamFrame)
	for i := 0; i+audio.UpstreamFrame <= len(s16); i += audio.UpstreamFrame {
		flags = append(flags, v.Process(s16[i:i+audio.UpstreamFrame]))
	}
	return flags
}

func TestFixturesVADSanity(t *testing.T) {
	for _, e := range audiofx.FixtureManifest(t) {
		t.Run(e.Name, func(t *testing.T) {
			pcm, _ := audiofx.LoadFixture(t, e.Name)
			flags := vadFlags(t, pcm)
			if len(e.Segments) == 0 {
				voiced := 0
				for _, f := range flags {
					if f {
						voiced++
					}
				}
				ratio := float64(voiced) / float64(len(flags))
				t.Logf("voiced %d/%d frames (%.1f%%)", voiced, len(flags), 100*ratio)
				if ratio > maxNoiseRatio {
					t.Fatalf("non-speech fixture voiced ratio %.3f > %.2f", ratio, maxNoiseRatio)
				}
				return
			}
			voiced, total := 0, 0
			for i, f := range flags {
				if e.SegmentCovers(i*frameMs, (i+1)*frameMs) {
					total++
					if f {
						voiced++
					}
				}
			}
			ratio := float64(voiced) / float64(total)
			t.Logf("speech segments: voiced %d/%d frames (%.1f%%)", voiced, total, 100*ratio)
			if ratio < minVoicedRatio {
				t.Fatalf("speech voiced ratio %.3f < %.2f", ratio, minVoicedRatio)
			}
			if e.Name == "ja_filler_pause" {
				runs := voicedRuns(flags)
				if len(runs) != 2 {
					t.Fatalf("want exactly 2 voiced runs, got %d: %v", len(runs), runs)
				}
				if gap := runs[1][0] - runs[0][1]; gap < minPauseFrames {
					t.Fatalf("voiced runs separated by %d silent frames, want ≥ %d", gap, minPauseFrames)
				}
			}
		})
	}
}

// voicedRuns returns [start, end) frame indices of consecutive voiced frames.
func voicedRuns(flags []bool) [][2]int {
	var runs [][2]int
	start := -1
	for i, f := range append(append([]bool(nil), flags...), false) {
		switch {
		case f && start < 0:
			start = i
		case !f && start >= 0:
			runs = append(runs, [2]int{start, i})
			start = -1
		}
	}
	return runs
}

func TestLoadFixtureUnknown(t *testing.T) {
	ft := &fakeT{TB: t}
	func() {
		defer func() { _ = recover() }()
		audiofx.LoadFixture(ft, "does_not_exist")
	}()
	if !ft.failed {
		t.Fatal("LoadFixture must fail the test for an unknown fixture")
	}
	if !strings.Contains(ft.msg, "does_not_exist") {
		t.Fatalf("failure message %q should name the fixture", ft.msg)
	}
}

// fakeT captures Fatalf without stopping the real test.
type fakeT struct {
	testing.TB
	failed bool
	msg    string
}

func (f *fakeT) Helper() {}

func (f *fakeT) Fatalf(format string, args ...any) {
	f.failed = true
	f.msg = fmt.Sprintf(format, args...)
	panic("fatal")
}
