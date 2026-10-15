// Package audiofx loads the §14.5 audio fixtures (testdata/audio/synth, plus native
// recordings in testdata/audio from M9). It imports the cgo package conversation/audio,
// so it lives apart from the cgo-free internal/testutil root (§14.9.5 rule 5 exception).
package audiofx

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nabePi/kotoki-backend/internal/conversation/audio"
	"github.com/nabePi/kotoki-backend/internal/testutil"
)

// SynthRel is the synthetic fixture directory relative to testdata/.
const SynthRel = "audio/synth"

// Segment is a speech span [StartMs, EndMs) inside a fixture.
type Segment struct {
	StartMs int `json:"start_ms"`
	EndMs   int `json:"end_ms"`
}

// Expect holds the expected outcome of feeding the fixture to the realtime core.
type Expect struct {
	Turns int `json:"turns"`
}

// Entry is one manifest.json fixture.
type Entry struct {
	Name       string    `json:"name"`
	DurationMs int       `json:"duration_ms"`
	Tags       []string  `json:"tags"`
	Expect     *Expect   `json:"expect"`
	Segments   []Segment `json:"segments,omitempty"`
}

// InSegment reports whether ms lies inside a speech segment.
func (e Entry) InSegment(ms int) bool {
	for _, s := range e.Segments {
		if ms >= s.StartMs && ms < s.EndMs {
			return true
		}
	}
	return false
}

// SegmentCovers reports whether [fromMs, toMs) lies entirely inside one speech segment.
func (e Entry) SegmentCovers(fromMs, toMs int) bool {
	for _, s := range e.Segments {
		if fromMs >= s.StartMs && toMs <= s.EndMs {
			return true
		}
	}
	return false
}

// Partial is one STT timeline step of <name>.partials.json.
type Partial struct {
	AtMs  int    `json:"at_ms"`
	Text  string `json:"text"`
	Final bool   `json:"final,omitempty"`
}

type manifest struct {
	Fixtures []Entry `json:"fixtures"`
}

// SynthDir returns <module>/testdata/audio/synth.
func SynthDir(t testing.TB) string {
	t.Helper()
	return testutil.TestdataPath(t, SynthRel)
}

// FixtureManifest reads testdata/audio/synth/manifest.json.
func FixtureManifest(t testing.TB) []Entry {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(SynthDir(t), "manifest.json"))
	if err != nil {
		t.Fatalf("audiofx: manifest: %v", err)
	}
	var m manifest
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("audiofx: manifest: %v", err)
	}
	if len(m.Fixtures) == 0 {
		t.Fatalf("audiofx: manifest lists no fixtures")
	}
	return m.Fixtures
}

// find looks for <name><ext>, preferring testdata/audio (native recordings) over
// testdata/audio/synth.
func find(t testing.TB, name, ext string) (string, bool) {
	t.Helper()
	root := filepath.Join(testutil.ModuleRoot(t), "testdata", "audio")
	for _, dir := range []string{root, filepath.Join(root, "synth")} {
		p := filepath.Join(dir, name+ext)
		if _, err := os.Stat(p); err == nil {
			return p, true
		}
	}
	return "", false
}

// Path returns the fixture file <name><ext> (testdata/audio first, then synth/);
// it fails when neither exists.
func Path(t testing.TB, name, ext string) string {
	t.Helper()
	p, ok := find(t, name, ext)
	if !ok {
		t.Fatalf("audiofx: fixture %s%s not found in testdata/audio or testdata/audio/synth", name, ext)
	}
	return p
}

// LoadFixture returns the fixture audio and its one-line transcript.
func LoadFixture(t testing.TB, name string) (audio.PCM, string) {
	t.Helper()
	pcm, err := audio.ReadWAV(Path(t, name, ".wav"))
	if err != nil {
		t.Fatalf("audiofx: %s: %v", name, err)
	}
	b, err := os.ReadFile(Path(t, name, ".txt"))
	if err != nil {
		t.Fatalf("audiofx: %s transcript: %v", name, err)
	}
	line, _, _ := strings.Cut(string(b), "\n")
	return pcm, strings.TrimSpace(line)
}

// PartialsPath returns <name>.partials.json, or "" when the fixture has none.
func PartialsPath(t testing.TB, name string) string {
	t.Helper()
	p, _ := find(t, name, ".partials.json")
	return p
}

// LoadPartials parses <name>.partials.json; a fixture without one yields nil.
func LoadPartials(t testing.TB, name string) []Partial {
	t.Helper()
	p := PartialsPath(t, name)
	if p == "" {
		return nil
	}
	b, err := os.ReadFile(p) //nolint:gosec // fixture path under testdata
	if err != nil {
		t.Fatalf("audiofx: %s partials: %v", name, err)
	}
	var parts []Partial
	if err := json.Unmarshal(b, &parts); err != nil {
		t.Fatalf("audiofx: %s partials: %v", name, err)
	}
	return parts
}
