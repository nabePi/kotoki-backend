// Command genaudio generates the deterministic synthetic Japanese-like audio fixtures of
// spec §14.5 into testdata/audio/synth: WAV PCM16 mono 48 kHz + <name>.txt (one-line
// transcript) + <name>.partials.json (STT timeline) + manifest.json.
//
// The audio is not intelligible speech. Each speech segment is a harmonic source
// (F0 120 Hz) shaped by three vowel formants that change every syllable, amplitude
// modulated at 4 Hz (one syllable per 250 ms). Pauses are digital zero, noise_only is
// red noise at −45 dBFS and silence_5s is all zeros. The output depends only on the
// fixed seed, so `make fixtures` reproduces the committed files.
//
// With -to16k <dir> it instead writes 16 kHz copies (Decimate48to16) plus the .txt
// sidecars of every WAV in -src, for FAKE_STT_FIXTURE_DIR (`make fixtures16k`).
// Dev tool; it may import the cgo audio package.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"hash/fnv"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"

	"github.com/nabePi/kotoki-backend/internal/conversation/audio"
	"github.com/nabePi/kotoki-backend/internal/provider"
)

const (
	rate = 48000
	seed = 0x6b6f746f6b69 // "kotoki"

	// Speech source.
	f0            = 120.0  // Hz, base pitch
	f0Wobble      = 0.04   // ±4 % slow pitch contour so it is not a steady drone
	f0WobbleHz    = 0.7    // contour rate
	questionRise  = 0.25   // +25 % pitch over the last riseMs of a question
	riseMs        = 400    //
	maxHarmonicHz = 3800.0 // WebRTC VAD analyses 0–4 kHz
	sylRate       = 4.0    // syllables per second (4 Hz amplitude modulation)
	envFloor      = 0.35   // syllable envelope never drops below this (≈ −9 dB)
	speechRMS     = 0.10   // per-syllable RMS of the harmonic sum (≈ −20 dBFS)
	fadeMs        = 15     // raised-cosine fade at segment edges (no clicks)

	// Noise.
	noiseDBFS = -45.0
	redPole   = 0.995 // leaky integrator → 1/f² spectrum above ~40 Hz
)

type kind int

const (
	kindSpeech kind = iota
	kindNoise
	kindSilence
)

// formant is one vowel: centre frequencies and bandwidths of F1..F3 (male Japanese).
type formant struct{ f, b [3]float64 }

var vowels = []formant{
	{f: [3]float64{750, 1200, 2600}, b: [3]float64{90, 110, 170}}, // a
	{f: [3]float64{300, 2200, 3000}, b: [3]float64{90, 110, 170}}, // i
	{f: [3]float64{350, 1350, 2400}, b: [3]float64{90, 110, 170}}, // u
	{f: [3]float64{480, 1900, 2550}, b: [3]float64{90, 110, 170}}, // e
	{f: [3]float64{500, 850, 2500}, b: [3]float64{90, 110, 170}},  // o
}

type segment struct {
	StartMs, EndMs int
	Rise           bool // question intonation at the end
}

type partial struct {
	AtMs  int    `json:"at_ms"`
	Text  string `json:"text"`
	Final bool   `json:"final,omitempty"`
}

type fixtureSpec struct {
	Name       string
	DurationMs int
	Tags       []string
	Kind       kind
	Segments   []segment
	Transcript string
	Partials   []partial
}

const (
	longText = "私は毎朝六時に起きて、近くの公園を三十分ぐらい散歩してから、コーヒーを飲みながらニュースを読みます。" +
		"それから電車で会社に行って、午前中は会議が多いので忙しいですが、" +
		"昼休みには同僚と一緒に駅の近くの店でラーメンを食べたりします。"
	maxText = "去年の秋に、大学時代の友達と三人で京都へ旅行に行きました。" +
		"朝早く東京駅から新幹線に乗って、お弁当を食べながら窓の外の富士山を見ていたら、あっという間に京都に着きました。" +
		"最初の日は清水寺と金閣寺を見に行きましたが、どこも観光客がとても多くて、写真を撮るのも大変でした。" +
		"夜は先斗町の小さなお店で湯豆腐と京料理を食べて、遅くまでいろいろな話をしました。" +
		"次の日は朝早く起きて、嵐山の竹林を散歩してから、川沿いのカフェで抹茶のパフェを食べました。" +
		"午後は伏見稲荷大社に行って、たくさんの赤い鳥居の下を山の上まで歩いたので、足がとても疲れましたが、景色はすばらしかったです。" +
		"また機会があったら、今度は春の桜の季節に行ってみたいと思っています。"
)

// fixtures is the §14.5 table (total 98.5 s).
var fixtures = []fixtureSpec{
	{
		Name: "ja_greeting_short", DurationMs: 1500, Tags: []string{"speech", "e1", "load"}, Kind: kindSpeech,
		Segments:   []segment{{StartMs: 250, EndMs: 1250}},
		Transcript: "こんにちは。",
		// No final: the turn engine must fall back to the default 800 ms threshold.
		Partials: []partial{{AtMs: 500, Text: "こん"}, {AtMs: 800, Text: "こんにち"}, {AtMs: 1150, Text: "こんにちは"}},
	},
	{
		Name: "ja_self_intro", DurationMs: 4000, Tags: []string{"speech", "e1", "live-stt"}, Kind: kindSpeech,
		Segments:   []segment{{StartMs: 200, EndMs: 3800}},
		Transcript: "はじめまして、田中です。よろしくお願いします。",
		Partials: []partial{
			{AtMs: 600, Text: "はじめ"}, {AtMs: 1200, Text: "はじめまして"}, {AtMs: 2000, Text: "はじめまして田中です"},
			{AtMs: 2800, Text: "はじめまして田中ですよろしく"}, {AtMs: 3600, Text: "はじめまして田中ですよろしくお願いします"},
			{AtMs: 3900, Text: "はじめまして、田中です。よろしくお願いします。", Final: true},
		},
	},
	{
		Name: "ja_filler_pause", DurationMs: 4000, Tags: []string{"speech", "turn-engine", "filler", "pause"}, Kind: kindSpeech,
		Segments:   []segment{{StartMs: 200, EndMs: 800}, {StartMs: 1800, EndMs: 3800}},
		Transcript: "えっと…週末は友達と映画を見ました。",
		Partials: []partial{
			{AtMs: 400, Text: "えっと"}, {AtMs: 2300, Text: "えっと週末は"}, {AtMs: 2900, Text: "えっと週末は友達と"},
			{AtMs: 3500, Text: "えっと週末は友達と映画を"}, {AtMs: 3750, Text: "えっと週末は友達と映画を見ました"},
			{AtMs: 3900, Text: "えっと…週末は友達と映画を見ました。", Final: true},
		},
	},
	{
		Name: "ja_conjunction_pause", DurationMs: 4000, Tags: []string{"speech", "turn-engine", "conjunction", "pause"}, Kind: kindSpeech,
		Segments:   []segment{{StartMs: 150, EndMs: 1550}, {StartMs: 2650, EndMs: 3850}},
		Transcript: "昨日は雨だったけど、散歩に行きました。",
		Partials: []partial{
			{AtMs: 500, Text: "昨日は"}, {AtMs: 1000, Text: "昨日は雨"}, {AtMs: 1450, Text: "昨日は雨だったけど"},
			{AtMs: 3100, Text: "昨日は雨だったけど散歩に"}, {AtMs: 3750, Text: "昨日は雨だったけど散歩に行きました"},
			{AtMs: 3950, Text: "昨日は雨だったけど、散歩に行きました。", Final: true},
		},
	},
	{
		Name: "ja_question", DurationMs: 2000, Tags: []string{"speech", "turn-engine", "question"}, Kind: kindSpeech,
		Segments:   []segment{{StartMs: 200, EndMs: 1600, Rise: true}},
		Transcript: "趣味は何ですか？",
		Partials: []partial{
			{AtMs: 500, Text: "趣味"}, {AtMs: 900, Text: "趣味は"}, {AtMs: 1300, Text: "趣味は何です"},
			{AtMs: 1550, Text: "趣味は何ですか"}, {AtMs: 1700, Text: "趣味は何ですか？", Final: true},
		},
	},
	{
		Name: "ja_long_utterance", DurationMs: 20000, Tags: []string{"speech", "load", "stt", "long"}, Kind: kindSpeech,
		Segments:   []segment{{StartMs: 200, EndMs: 19800}},
		Transcript: longText,
	},
	{
		Name: "ja_max_utterance", DurationMs: 50000, Tags: []string{"speech", "turn-engine", "max-utterance", "long"}, Kind: kindSpeech,
		Segments:   []segment{{StartMs: 200, EndMs: 49800}},
		Transcript: maxText,
	},
	{
		Name: "ja_barge_in", DurationMs: 3000, Tags: []string{"speech", "e3", "load", "barge-in"}, Kind: kindSpeech,
		Segments:   []segment{{StartMs: 200, EndMs: 2800}},
		Transcript: "あ、すみません、ちょっと待ってください。",
		Partials: []partial{
			{AtMs: 400, Text: "あ"}, {AtMs: 700, Text: "あすみ"}, {AtMs: 1000, Text: "あすみません"},
			{AtMs: 1800, Text: "あすみませんちょっと"}, {AtMs: 2600, Text: "あすみませんちょっと待ってください"},
			{AtMs: 2900, Text: "あ、すみません、ちょっと待ってください。", Final: true},
		},
	},
	{Name: "noise_only", DurationMs: 5000, Tags: []string{"noise", "vad"}, Kind: kindNoise, Transcript: "（雑音のみ）"},
	{Name: "silence_5s", DurationMs: 5000, Tags: []string{"silence", "pacing"}, Kind: kindSilence, Transcript: "（無音）"},
}

func init() {
	for i := range fixtures {
		f := &fixtures[i]
		if f.Kind == kindSpeech && f.Partials == nil {
			f.Partials = spreadPartials(f.Transcript, f.Segments[0])
		}
	}
}

// spreadPartials builds a cumulative partial timeline for a continuous segment: one
// partial per clause (split after 、 and 。), evenly spaced, then the final.
func spreadPartials(text string, seg segment) []partial {
	var clauses []string
	cur := ""
	for _, r := range text {
		cur += string(r)
		if r == '、' || r == '。' {
			clauses = append(clauses, cur)
			cur = ""
		}
	}
	if cur != "" {
		clauses = append(clauses, cur)
	}
	span := seg.EndMs - seg.StartMs
	out := make([]partial, 0, len(clauses)+1)
	prefix := ""
	for i, c := range clauses {
		prefix += c
		at := seg.StartMs + (i+1)*span/len(clauses) - 100
		out = append(out, partial{AtMs: at, Text: strings.TrimRight(prefix, "、。")})
	}
	return append(out, partial{AtMs: seg.EndMs + 100, Text: text, Final: true})
}

func newRNG(name string) *rand.Rand {
	h := fnv.New64a()
	_, _ = h.Write([]byte(name))
	return rand.New(rand.NewPCG(seed, h.Sum64())) //nolint:gosec // deterministic fixture noise, not crypto
}

// render produces the fixture's 48 kHz samples.
func render(f fixtureSpec) []int16 {
	out := make([]int16, f.DurationMs*rate/1000)
	rng := newRNG(f.Name)
	switch f.Kind {
	case kindSpeech:
		for _, s := range f.Segments {
			renderSpeech(out[s.StartMs*rate/1000:s.EndMs*rate/1000], s.Rise, rng)
		}
	case kindNoise:
		renderRedNoise(out, rng)
	case kindSilence:
	}
	return out
}

// harmonicWeights returns the amplitude of harmonics 1..n of pitch p shaped by a
// −6 dB/octave source and three second-order formant resonances, scaled so the
// harmonic sum has RMS speechRMS.
func harmonicWeights(v formant, p float64, n int) []float64 {
	w := make([]float64, n)
	energy := 0.0
	for h := range w {
		fh := float64(h+1) * p
		g := 1 / float64(h+1)
		for k := range 3 {
			fk, bk := v.f[k], v.b[k]
			g *= fk * fk / math.Sqrt((fk*fk-fh*fh)*(fk*fk-fh*fh)+(fh*bk)*(fh*bk))
		}
		w[h] = g
		energy += g * g / 2
	}
	scale := speechRMS / math.Sqrt(energy)
	for h := range w {
		w[h] *= scale
	}
	return w
}

func renderSpeech(out []int16, rise bool, rng *rand.Rand) {
	n := len(out)
	nSyl := max(1, int(math.Round(float64(n)/rate*sylRate)))
	sylLen := float64(n) / float64(nSyl)
	nh := int(math.Floor(maxHarmonicHz / f0))
	fade := fadeMs * rate / 1000
	riseLen := riseMs * rate / 1000
	var w []float64
	syl, phase := -1, 0.0
	for i := range out {
		t := float64(i) / rate
		p := f0 * (1 + f0Wobble*math.Sin(2*math.Pi*f0WobbleHz*t))
		if rem := n - i; rise && rem < riseLen {
			p *= 1 + questionRise*float64(riseLen-rem)/float64(riseLen)
		}
		k := min(int(float64(i)/sylLen), nSyl-1)
		if k != syl {
			syl = k
			w = harmonicWeights(vowels[rng.IntN(len(vowels))], p, nh)
		}
		phase = math.Mod(phase+2*math.Pi*p/rate, 2*math.Pi)
		// sin(hφ) by the Chebyshev recurrence sin((h+1)φ) = 2cosφ·sin(hφ) − sin((h−1)φ).
		c2 := 2 * math.Cos(phase)
		prev, cur := 0.0, math.Sin(phase)
		sum := w[0] * cur
		for h := 1; h < nh; h++ {
			prev, cur = cur, c2*cur-prev
			sum += w[h] * cur
		}
		frac := (float64(i) - float64(k)*sylLen) / sylLen
		s := math.Sin(math.Pi * frac)
		env := envFloor + (1-envFloor)*s*s
		if i < fade {
			env *= 0.5 - 0.5*math.Cos(math.Pi*float64(i)/float64(fade))
		}
		if j := n - 1 - i; j < fade {
			env *= 0.5 - 0.5*math.Cos(math.Pi*float64(j)/float64(fade))
		}
		out[i] = toInt16(sum * env)
	}
}

// renderRedNoise fills out with zero-mean red (Brownian) noise at noiseDBFS RMS.
func renderRedNoise(out []int16, rng *rand.Rand) {
	x := make([]float64, len(out))
	y, mean := 0.0, 0.0
	for i := range x {
		y = redPole*y + (2*rng.Float64() - 1)
		x[i] = y
		mean += y
	}
	mean /= float64(len(x))
	energy := 0.0
	for i := range x {
		x[i] -= mean
		energy += x[i] * x[i]
	}
	scale := math.Pow(10, noiseDBFS/20) / math.Sqrt(energy/float64(len(x)))
	for i := range x {
		out[i] = toInt16(x[i] * scale)
	}
}

func toInt16(v float64) int16 {
	return int16(math.Max(-32768, math.Min(32767, math.Round(v*32767))))
}

type manifestSegment struct {
	StartMs int `json:"start_ms"`
	EndMs   int `json:"end_ms"`
}

type manifestEntry struct {
	Name       string            `json:"name"`
	DurationMs int               `json:"duration_ms"`
	Tags       []string          `json:"tags"`
	Expect     map[string]int    `json:"expect"`
	Segments   []manifestSegment `json:"segments,omitempty"`
}

func manifestJSON(specs []fixtureSpec) ([]byte, error) {
	entries := make([]manifestEntry, 0, len(specs))
	for _, f := range specs {
		e := manifestEntry{Name: f.Name, DurationMs: f.DurationMs, Tags: f.Tags, Expect: map[string]int{"turns": 0}}
		if f.Kind == kindSpeech {
			e.Expect["turns"] = 1
		}
		for _, s := range f.Segments {
			e.Segments = append(e.Segments, manifestSegment{StartMs: s.StartMs, EndMs: s.EndMs})
		}
		entries = append(entries, e)
	}
	return marshal(map[string]any{"fixtures": entries})
}

func marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeFile(path string, b []byte) error {
	return os.WriteFile(path, b, 0o644) //nolint:gosec // fixtures are public test data
}

// writeAll renders specs into dir with their sidecars and manifest.json.
func writeAll(dir string, specs []fixtureSpec) error {
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // public test data directory
		return err
	}
	for _, f := range specs {
		wav := provider.EncodeWAV(audio.Int16ToBytes(render(f), nil), rate)
		if err := writeFile(filepath.Join(dir, f.Name+".wav"), wav); err != nil {
			return err
		}
		if err := writeFile(filepath.Join(dir, f.Name+".txt"), []byte(f.Transcript+"\n")); err != nil {
			return err
		}
		if len(f.Partials) > 0 {
			b, err := marshal(f.Partials)
			if err != nil {
				return err
			}
			if err := writeFile(filepath.Join(dir, f.Name+".partials.json"), b); err != nil {
				return err
			}
		}
	}
	m, err := manifestJSON(specs)
	if err != nil {
		return err
	}
	return writeFile(filepath.Join(dir, "manifest.json"), m)
}

// to16k writes a 16 kHz copy (Decimate48to16) and the .txt sidecar of every 48 kHz
// WAV in src into dst.
func to16k(src, dst string) error {
	wavs, err := filepath.Glob(filepath.Join(src, "*.wav"))
	if err != nil {
		return err
	}
	if len(wavs) == 0 {
		return fmt.Errorf("no *.wav in %s", src)
	}
	if err := os.MkdirAll(dst, 0o755); err != nil { //nolint:gosec // dev output directory
		return err
	}
	for _, w := range wavs {
		pcm, err := audio.ReadWAV(w)
		if err != nil {
			return fmt.Errorf("%s: %w", filepath.Base(w), err)
		}
		if pcm.Rate != rate {
			return fmt.Errorf("%s: rate %d, want %d", filepath.Base(w), pcm.Rate, rate)
		}
		base := strings.TrimSuffix(filepath.Base(w), ".wav")
		txt, err := os.ReadFile(filepath.Join(src, base+".txt")) //nolint:gosec // dev-provided fixture directory
		if err != nil {
			return fmt.Errorf("%s has no .txt sidecar: %w", base, err)
		}
		out := provider.EncodeWAV(audio.Int16ToBytes(audio.Decimate48to16(pcm.Samples), nil), audio.UpstreamRate)
		if err := writeFile(filepath.Join(dst, base+".wav"), out); err != nil {
			return err
		}
		if err := writeFile(filepath.Join(dst, base+".txt"), txt); err != nil {
			return err
		}
	}
	return nil
}

func main() {
	out := flag.String("out", "testdata/audio/synth", "output directory for the 48 kHz fixtures")
	dst16 := flag.String("to16k", "", "instead of generating, write 16 kHz copies of -src into this directory")
	src := flag.String("src", "testdata/audio/synth", "48 kHz fixture directory read by -to16k")
	flag.Parse()
	var err error
	if *dst16 != "" {
		err = to16k(*src, *dst16)
	} else {
		err = writeAll(*out, fixtures)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "genaudio:", err)
		os.Exit(1)
	}
}
