// Package livekit holds the helpers shared by the live (`-tags live`, real vendor keys) tests of
// the provider adapters (M9-T16): real-speaker fixture loading, a STT driver, the CER metric,
// the `-record` flag that turns a live run into replay fixtures, and the scenario checks shared
// by more than one vendor (LLM, evaluation). It has no build tag so that CER stays covered by
// `make test` (TestCER in internal/provider). Credentials are read by the callers only through
// testutil.RequireEnv; nothing here reads an API key.
package livekit

import (
	"context"
	"flag"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/provider/httpx"
	"github.com/nabePi/kotoki-backend/internal/testutil"
	"github.com/nabePi/kotoki-backend/internal/testutil/replay"
)

// Record turns a live run into fixtures: the adapters are wired to a replay.Recorder that writes
// internal/provider/<vendor>/testdata/recordings/<cap>_live_<case>.jsonl with meta.verified=true.
var Record = flag.Bool("record", false, "live tests: record vendor traffic as replay fixtures (verified=true)")

// STTFixtures are the fixture names of §14.5; only real-speaker recordings qualify.
var STTFixtures = []string{"ja_greeting_short", "ja_self_intro", "ja_question", "ja_filler_pause", "ja_long_utterance"}

// Getenv returns the value of key or def when it is empty; for endpoints, never for credentials.
func Getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// LoadPCM16k reads testdata/audio/<name>.wav (a real-speaker recording, never synth/) as PCM16LE
// mono 16 kHz. The test is skipped, with a message that says what to record, when it is missing.
func LoadPCM16k(t testing.TB, name string) []byte {
	t.Helper()
	path := filepath.Join(testutil.ModuleRoot(t), "testdata", "audio", name+".wav")
	raw, err := os.ReadFile(path) //nolint:gosec // fixture path built from a fixed name list
	if os.IsNotExist(err) {
		t.Skipf("MANUAL GATE: real-speaker fixture testdata/audio/%s.wav (+ .txt) is not recorded yet; "+
			"testdata/audio/synth/ is synthetic and is never used by live tests", name)
	}
	if err != nil {
		t.Fatalf("livekit: %v", err)
	}
	pcm, rate, err := provider.DecodeWAV(raw)
	if err != nil {
		t.Fatalf("livekit: %s.wav: %v", name, err)
	}
	if rate != provider.STTSampleRate {
		pcm = provider.ResamplePCM16(pcm, rate, provider.STTSampleRate)
	}
	return pcm
}

// LoadRef reads the reference transcript testdata/audio/<name>.txt (skip when missing).
func LoadRef(t testing.TB, name string) string {
	t.Helper()
	path := filepath.Join(testutil.ModuleRoot(t), "testdata", "audio", name+".txt")
	raw, err := os.ReadFile(path) //nolint:gosec // fixture path built from a fixed name list
	if os.IsNotExist(err) {
		t.Skipf("MANUAL GATE: reference transcript testdata/audio/%s.txt is missing", name)
	}
	if err != nil {
		t.Fatalf("livekit: %v", err)
	}
	return strings.TrimSpace(string(raw))
}

// Transcribe plays pcm (16 kHz, 20 ms chunks at four times real time) into a new session of p,
// finalizes, and returns the concatenated FINAL texts. A vendor ERROR fails the test.
func Transcribe(t testing.TB, p provider.STTProvider, pcm []byte) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	s, err := p.Open(ctx, provider.STTSessionConfig{Language: "ja", SampleRate: provider.STTSampleRate})
	if err != nil {
		t.Fatalf("livekit: Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	const chunk = 640 // 20 ms
	for off := 0; off < len(pcm); off += chunk {
		end := min(off+chunk, len(pcm))
		if err := s.SendAudio(pcm[off:end]); err != nil {
			t.Fatalf("livekit: SendAudio: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := s.Finalize(); err != nil {
		t.Fatalf("livekit: Finalize: %v", err)
	}
	var finals []string
	timeout := time.After(30 * time.Second)
	for {
		select {
		case ev, ok := <-s.Events():
			if !ok {
				t.Fatal("livekit: events closed before the FINAL of Finalize")
			}
			switch ev.Type {
			case provider.STTError:
				t.Fatalf("livekit: STT error: %v", ev.Err)
			case provider.STTFinal:
				finals = append(finals, ev.Text)
				if ev.FromFinalize {
					return strings.Join(finals, "")
				}
			case provider.STTPartial, provider.STTClosed:
			}
		case <-timeout:
			t.Fatal("livekit: no FINAL after Finalize within 30 s")
		}
	}
}

// ignoredInCER are dropped before comparing: spaces and Japanese/ASCII punctuation.
const ignoredInCER = " \t\r\n　。、？！?!,.・「」"

// CER is the character error rate of hyp against ref: the rune Levenshtein distance after
// removing spaces and 。、？！?!,.・「」, divided by the rune length of ref. An empty ref gives 0
// for an empty hyp and 1 otherwise.
func CER(hyp, ref string) float64 {
	h, r := stripCER(hyp), stripCER(ref)
	if len(r) == 0 {
		if len(h) == 0 {
			return 0
		}
		return 1
	}
	prev := make([]int, len(r)+1)
	cur := make([]int, len(r)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(h); i++ {
		cur[0] = i
		for j := 1; j <= len(r); j++ {
			cost := 1
			if h[i-1] == r[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return float64(prev[len(r)]) / float64(len(r))
}

func stripCER(s string) []rune {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if !strings.ContainsRune(ignoredInCER, r) {
			out = append(out, r)
		}
	}
	return out
}

// NewRecorder returns the recorder of <vendor>/<capability>_live_<name>.jsonl, flushed when the
// test ends, or nil when -record is off.
func NewRecorder(t testing.TB, vendor, capability, name string) *replay.Recorder {
	t.Helper()
	if !*Record {
		return nil
	}
	path := filepath.Join(replay.RecordingsDir(t, vendor), capability+"_live_"+name+".jsonl")
	rec := replay.NewRecorder(t, vendor, capability, path)
	t.Cleanup(rec.Flush)
	return rec
}

// HTTPClient is httpx.NewClient, recorded when rec is not nil.
func HTTPClient(rec *replay.Recorder) *http.Client {
	hc := httpx.NewClient()
	if rec != nil {
		hc.Transport = rec.Transport(hc.Transport)
	}
	return hc
}

// Tap is the adapter Config.Tap of rec, nil when rec is nil.
func Tap(rec *replay.Recorder) func(dir string, text bool, data []byte) {
	if rec == nil {
		return nil
	}
	return rec.Tap
}
