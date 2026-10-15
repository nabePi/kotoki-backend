package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/nabePi/kotoki-backend/internal/conversation/audio"
	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/testutil"
)

func spec(t *testing.T, name string) fixtureSpec {
	t.Helper()
	for _, f := range fixtures {
		if f.Name == name {
			return f
		}
	}
	t.Fatalf("no fixture spec %q", name)
	return fixtureSpec{}
}

func TestFixtureTableTotals(t *testing.T) {
	if len(fixtures) != 10 {
		t.Fatalf("want the 10 §14.5 fixtures, got %d", len(fixtures))
	}
	total := 0
	for _, f := range fixtures {
		total += f.DurationMs
	}
	if total != 98500 {
		t.Fatalf("total duration %d ms, want 98500", total)
	}
}

func TestRenderDeterministic(t *testing.T) {
	for _, name := range []string{"ja_greeting_short", "ja_question", "noise_only", "silence_5s"} {
		f := spec(t, name)
		a, b := render(f), render(f)
		if len(a) != f.DurationMs*rate/1000 {
			t.Fatalf("%s: %d samples, want %d", name, len(a), f.DurationMs*rate/1000)
		}
		if !slices.Equal(a, b) {
			t.Fatalf("%s: two renders differ", name)
		}
	}
}

// TestRenderMatchesCommitted guards against generator drift: re-rendering must
// reproduce testdata/audio/synth (±1 LSB tolerates FMA differences across CPUs).
func TestRenderMatchesCommitted(t *testing.T) {
	dir := testutil.TestdataPath(t, "audio/synth")
	for _, f := range fixtures {
		if f.DurationMs > 5000 {
			continue // long fixtures are covered by the same code path
		}
		got := render(f)
		pcm, err := audio.ReadWAV(filepath.Join(dir, f.Name+".wav"))
		if err != nil {
			t.Fatal(err)
		}
		if len(pcm.Samples) != len(got) {
			t.Fatalf("%s: committed %d samples, render %d (run make fixtures)", f.Name, len(pcm.Samples), len(got))
		}
		for i := range got {
			if d := int(got[i]) - int(pcm.Samples[i]); d < -1 || d > 1 {
				t.Fatalf("%s: sample %d differs (%d vs %d); run make fixtures", f.Name, i, got[i], pcm.Samples[i])
			}
		}
		txt, err := os.ReadFile(filepath.Join(dir, f.Name+".txt"))
		if err != nil || string(txt) != f.Transcript+"\n" {
			t.Fatalf("%s: committed transcript %q differs from spec %q (err %v)", f.Name, txt, f.Transcript, err)
		}
	}
	committed, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	want, err := manifestJSON(fixtures)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(committed, want) {
		t.Fatal("committed manifest.json differs from the generator table; run make fixtures")
	}
}

func TestWriteAndTo16k(t *testing.T) {
	src, dst := t.TempDir(), filepath.Join(t.TempDir(), "fixtures16k")
	subset := []fixtureSpec{spec(t, "ja_filler_pause"), spec(t, "silence_5s")}
	if err := writeAll(src, subset); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"manifest.json", "ja_filler_pause.wav", "ja_filler_pause.txt", "ja_filler_pause.partials.json", "silence_5s.wav", "silence_5s.txt"} {
		if _, err := os.Stat(filepath.Join(src, f)); err != nil {
			t.Fatalf("writeAll did not produce %s: %v", f, err)
		}
	}
	if _, err := os.Stat(filepath.Join(src, "silence_5s.partials.json")); err == nil {
		t.Fatal("non-speech fixture must not get a partials file")
	}
	var parts []map[string]any
	b, err := os.ReadFile(filepath.Join(src, "ja_filler_pause.partials.json"))
	if err != nil || json.Unmarshal(b, &parts) != nil || len(parts) == 0 || parts[0]["at_ms"] != 400.0 || parts[0]["text"] != "えっと" {
		t.Fatalf("partials.json format: %s (err %v)", b, err)
	}

	if err := to16k(src, dst); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dst)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 4 {
		t.Fatalf("to16k wrote %d files, want 4 (wav+txt per fixture)", len(entries))
	}
	for _, f := range subset {
		raw, err := os.ReadFile(filepath.Join(dst, f.Name+".wav"))
		if err != nil {
			t.Fatal(err)
		}
		pcm, sr, err := provider.DecodeWAV(raw)
		if err != nil || sr != 16000 {
			t.Fatalf("%s: 16 kHz copy unreadable by fake STT (rate %d, err %v)", f.Name, sr, err)
		}
		if want := f.DurationMs * 16 * 2; len(pcm) != want {
			t.Fatalf("%s: 16 kHz copy has %d bytes, want %d", f.Name, len(pcm), want)
		}
		txt, err := os.ReadFile(filepath.Join(dst, f.Name+".txt"))
		if err != nil || string(txt) != f.Transcript+"\n" {
			t.Fatalf("%s: transcript copy %q (err %v)", f.Name, txt, err)
		}
	}
}
