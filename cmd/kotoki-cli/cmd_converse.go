package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/conversation/protocol"
	"github.com/nabePi/kotoki-backend/internal/kotokiclient"
	"github.com/nabePi/kotoki-backend/internal/kotokiclient/audio"
)

// afterSessionMargin is how long `--end wait-expiry` waits beyond the session length for
// DURATION_ELAPSED (the server adds a grace period, §7.11).
const afterSessionMargin = 30 * time.Second

type endKind int

const (
	endAfterLast endKind = iota
	endWaitExpiry
	endAfterDuration
)

type endMode struct {
	kind endKind
	d    time.Duration
}

// parseEnd reads `--end`: after-last, wait-expiry or a duration such as 90s.
func parseEnd(s string) (endMode, error) {
	switch s {
	case "after-last":
		return endMode{kind: endAfterLast}, nil
	case "wait-expiry":
		return endMode{kind: endWaitExpiry}, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return endMode{}, usagef("--end must be after-last, wait-expiry or a positive duration such as 90s, got %q", s)
	}
	return endMode{kind: endAfterDuration, d: d}, nil
}

// plan is everything that shapes the script of a conversation.
type plan struct {
	utterances [][][]byte
	labels     []string
	aiFirst    bool
	think      time.Duration
	waitAI     bool
	aiTimeout  time.Duration
	bargeAfter time.Duration // 0: off
	dropAfter  time.Duration // 0: off
	dropFor    time.Duration
	end        endMode
	sessionLen time.Duration
}

// buildSteps turns the flags into a script (§14.11.2).
//
//   - AI_FIRST sessions start with WaitAIDone: the AI speaks first, and a Speak right away would
//     take the greeting for the answer to it.
//   - Between utterances: WaitAIDone (--wait-ai) then Think (--think). With --barge-in-after the
//     second and later utterances are BargeIn steps, which wait for the AI's first audio
//     themselves, so nothing is inserted before them.
//   - The end follows --end.
//   - --drop-after places one Drop at that point of the script's own timeline (see insertDrop).
func buildSteps(p plan) []kotokiclient.Step {
	var steps []kotokiclient.Step
	if p.aiFirst {
		steps = append(steps, kotokiclient.WaitAIDone{Timeout: p.aiTimeout})
	}
	last := len(p.utterances) - 1
	for i, pk := range p.utterances {
		if i > 0 && p.bargeAfter > 0 {
			steps = append(steps, kotokiclient.BargeIn{After: p.bargeAfter, Packets: pk, Timeout: p.aiTimeout})
		} else {
			steps = append(steps, kotokiclient.Speak{Packets: pk, Label: p.labels[i]})
		}
		if i == last {
			break
		}
		if p.bargeAfter > 0 {
			continue // the next BargeIn waits for the AI's audio itself
		}
		if p.waitAI {
			steps = append(steps, kotokiclient.WaitAIDone{Timeout: p.aiTimeout})
		}
		if p.think > 0 {
			steps = append(steps, kotokiclient.Think{D: p.think})
		}
	}
	switch p.end.kind {
	case endAfterLast:
		steps = append(steps, kotokiclient.WaitAIDone{Timeout: p.aiTimeout}, kotokiclient.End{Timeout: p.aiTimeout})
	case endWaitExpiry:
		steps = append(steps, kotokiclient.WaitAIDone{Timeout: p.aiTimeout}, kotokiclient.Think{D: p.sessionLen + afterSessionMargin})
	case endAfterDuration:
		steps = append(steps, kotokiclient.Think{D: p.end.d}, kotokiclient.End{Timeout: p.aiTimeout})
	}
	if p.dropAfter > 0 {
		steps = insertDrop(steps, p.dropAfter, p.dropFor)
	}
	return steps
}

// stepSpan is the time a step takes by itself: what the script sends. The wait for the AI is not
// predictable and counts as zero.
func stepSpan(s kotokiclient.Step) time.Duration {
	switch st := s.(type) {
	case kotokiclient.Speak:
		return time.Duration(len(st.Packets)) * kotokiclient.FrameInterval
	case kotokiclient.BargeIn:
		return st.After + time.Duration(len(st.Packets))*kotokiclient.FrameInterval
	case kotokiclient.Think:
		return st.D
	}
	return 0
}

// insertDrop puts a Drop{For: dropFor} into the script where `after` has elapsed on the script's
// own timeline (the sum of stepSpan): inside a Think or a Speak that spans the moment (the step
// is split), after a BargeIn that does, or before the End when the script is shorter than that.
func insertDrop(steps []kotokiclient.Step, after, dropFor time.Duration) []kotokiclient.Step {
	drop := kotokiclient.Drop{For: dropFor}
	out := make([]kotokiclient.Step, 0, len(steps)+2)
	var elapsed time.Duration
	placed := false
	for _, s := range steps {
		if placed {
			out = append(out, s)
			continue
		}
		if _, isEnd := s.(kotokiclient.End); isEnd || elapsed >= after {
			out = append(out, drop, s)
			placed = true
			continue
		}
		span := stepSpan(s)
		if elapsed+span <= after {
			out = append(out, s)
			elapsed += span
			continue
		}
		offset := after - elapsed // 0 < offset < span: the drop falls inside this step
		placed = true
		switch st := s.(type) {
		case kotokiclient.Think:
			out = append(out, kotokiclient.Think{D: offset}, drop, kotokiclient.Think{D: st.D - offset})
		case kotokiclient.Speak:
			n := int(offset / kotokiclient.FrameInterval)
			if n > 0 {
				out = append(out, kotokiclient.Speak{Packets: st.Packets[:n], Label: st.Label})
			}
			out = append(out, drop)
			if n < len(st.Packets) {
				out = append(out, kotokiclient.Speak{Packets: st.Packets[n:], Label: st.Label})
			}
		default: // BargeIn: not split
			out = append(out, s, drop)
		}
	}
	if !placed {
		out = append(out, drop)
	}
	return out
}

// converseFlags are the flags of `converse` (§14.11.2).
type converseFlags struct {
	session    string
	wav        string
	think      time.Duration
	waitAI     bool
	aiTimeout  time.Duration
	bargeAfter time.Duration
	dropAfter  time.Duration
	dropFor    time.Duration
	end        string
	outDir     string
	noAudio    bool
	quiet      bool
	sf         sessionFlags
}

func (c *converseFlags) bind(fs *flag.FlagSet) {
	c.sf.bind(fs)
	fs.StringVar(&c.session, "session", "", "use this RESERVED session; empty creates one from the session flags")
	fs.StringVar(&c.wav, "wav", "", "comma-separated WAV files, spoken in order (required)")
	fs.DurationVar(&c.think, "think", time.Second, "silence after the AI finishes, before the next utterance")
	fs.BoolVar(&c.waitAI, "wait-ai", true, "wait for the AI to finish before the next utterance")
	fs.DurationVar(&c.aiTimeout, "ai-timeout", 20*time.Second, "how long to wait for the AI to finish")
	fs.DurationVar(&c.bargeAfter, "barge-in-after", 0, "from the 2nd utterance on: start speaking this long after the AI's first audio frame (0: off)")
	fs.DurationVar(&c.dropAfter, "drop-after", 0, "drop the connection this long after start (0: off)")
	fs.DurationVar(&c.dropFor, "drop-for", 0, "stay disconnected this long")
	fs.StringVar(&c.end, "end", "after-last", "after-last, wait-expiry or a duration such as 90s")
	fs.StringVar(&c.outDir, "out-dir", "", "output directory (default out/<session_id>)")
	fs.BoolVar(&c.noAudio, "no-audio-save", false, "do not decode and save the AI audio")
	fs.BoolVar(&c.quiet, "quiet", false, "print only the summary")
}

func (a *app) cmdConverse(ctx context.Context, args []string) error {
	fs := a.newFlagSet("converse")
	var cf converseFlags
	cf.bind(fs)
	if err := a.parse(fs, args); err != nil {
		return err
	}
	var files []string
	for _, f := range strings.Split(cf.wav, ",") {
		if f = strings.TrimSpace(f); f != "" {
			files = append(files, f)
		}
	}
	if len(files) == 0 {
		return usagef("converse: --wav is required (one or more WAV files, comma-separated)")
	}
	end, err := parseEnd(cf.end)
	if err != nil {
		return err
	}
	if cf.session == "" {
		if err := cf.sf.validate(); err != nil {
			return err
		}
	}
	var existing uuid.UUID
	if cf.session != "" {
		if existing, err = parseSessionID(cf.session); err != nil {
			return err
		}
	}
	p := plan{
		think: cf.think, waitAI: cf.waitAI, aiTimeout: cf.aiTimeout, bargeAfter: cf.bargeAfter,
		dropAfter: cf.dropAfter, dropFor: cf.dropFor, end: end,
	}
	for _, f := range files {
		pcm, err := audio.ReadWAV(f)
		if err != nil {
			return fmt.Errorf("%s: %w", f, err)
		}
		pk, err := audio.PreEncode(pcm)
		if err != nil {
			return fmt.Errorf("%s: %w", f, err)
		}
		p.utterances = append(p.utterances, pk)
		p.labels = append(p.labels, filepath.Base(f))
	}

	cc, err := a.open()
	if err != nil {
		return err
	}
	var s kotokiclient.Session
	if cf.session != "" {
		if err := cc.run(ctx, func() (e error) { s, e = cc.GetSession(ctx, existing); return e }); err != nil {
			return err
		}
	} else if s, err = a.createSession(ctx, cc, cf.sf); err != nil {
		return err
	}
	p.aiFirst = s.ConversationStartMode == "AI_FIRST"
	p.sessionLen = time.Duration(s.DurationSeconds) * time.Second

	dir := cf.outDir
	if dir == "" {
		dir = filepath.Join("out", s.ID.String())
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	sink, err := newCLISink(a, dir, cf.quiet, time.Now())
	if err != nil {
		return err
	}
	collector := kotokiclient.NewAudioCollector()
	sinks := []kotokiclient.Sink{sink}
	if !cf.noAudio {
		sinks = append(sinks, collector)
	}
	if !cf.quiet && !a.g.JSON {
		a.printf("session %s (%s, %ds)\n", s.ID, s.ConversationStartMode, s.DurationSeconds)
	}

	rep, runErr := kotokiclient.RunConversation(ctx, cc.Client, kotokiclient.SessionSpec{
		SessionID: s.ID, WSURL: a.g.WSURL, Origin: a.g.Origin,
		Client: &protocol.ClientInfo{Platform: "cli", AppVersion: "kotoki-cli"},
	}, buildSteps(p), kotokiclient.MultiSink(sinks...))
	if perr := cc.persist(""); perr != nil && runErr == nil {
		runErr = perr
	}
	closeErr := sink.close()

	var saved []string
	if !cf.noAudio {
		var werr error
		if saved, werr = saveAudio(dir, collector); werr != nil && runErr == nil {
			runErr = werr
		}
	}
	if err := writeSummary(dir, s.ID, rep); err != nil && runErr == nil {
		runErr = err
	}
	if closeErr != nil && runErr == nil {
		runErr = closeErr
	}
	a.printSummary(s.ID, rep, dir, saved)
	return runErr
}

// cliSink records every event to events.jsonl and prints what happens as it happens.
type cliSink struct {
	a     *app
	f     *os.File
	t0    time.Time
	quiet bool

	mu       sync.Mutex
	firstGen map[uint32]bool
	werr     error
}

func newCLISink(a *app, dir string, quiet bool, t0 time.Time) (*cliSink, error) {
	f, err := os.OpenFile(filepath.Join(dir, "events.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec // G304: the directory is the user's own --out-dir
	if err != nil {
		return nil, err
	}
	return &cliSink{a: a, f: f, t0: t0, quiet: quiet, firstGen: map[uint32]bool{}}, nil
}

// eventRecord is one line of events.jsonl: the event as the server sent it plus the receive time.
type eventRecord struct {
	Kind       string          `json:"kind,omitempty"` // only on stdout
	ReceivedAt time.Time       `json:"received_at"`
	Event      json.RawMessage `json:"event"`
}

// OnEvent implements kotokiclient.Sink.
func (s *cliSink) OnEvent(e kotokiclient.Received[protocol.ServerEvent]) {
	raw, err := protocol.MarshalServer(e.Value)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.werr = errors.Join(s.werr, err)
		return
	}
	rec := eventRecord{ReceivedAt: e.At.UTC(), Event: raw}
	line, err := json.Marshal(rec)
	if err == nil {
		_, err = s.f.Write(append(line, '\n'))
	}
	s.werr = errors.Join(s.werr, err)
	if s.quiet {
		return
	}
	if s.a.g.JSON {
		rec.Kind = "event"
		_ = s.a.printJSON(rec)
		return
	}
	s.a.printf("%s  ← %s%s\n", s.stamp(e.At), e.Value.Type, eventDetail(e.Value))
}

// OnAudio implements kotokiclient.Sink: only the first frame of a generation is announced.
func (s *cliSink) OnAudio(f kotokiclient.AudioFrame) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.quiet || s.a.g.JSON || s.firstGen[f.Generation] {
		return
	}
	s.firstGen[f.Generation] = true
	s.a.printf("%s  ← [audio] gen=%d first frame\n", s.stamp(f.At), f.Generation)
}

func (s *cliSink) stamp(at time.Time) string {
	return fmt.Sprintf("+%.3fs", at.Sub(s.t0).Seconds())
}

func (s *cliSink) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return errors.Join(s.werr, s.f.Close())
}

// eventDetail is the part of an event worth a line of text: transcripts (printed locally only).
func eventDetail(ev protocol.ServerEvent) string {
	switch p := ev.Payload.(type) {
	case protocol.STTPartial:
		return fmt.Sprintf("  %q", p.Text)
	case protocol.STTFinal:
		return fmt.Sprintf("  %q", p.Text)
	case protocol.AISentence:
		return fmt.Sprintf("  %q", p.Text)
	case protocol.TurnCompleted:
		return "  " + p.Status
	case protocol.ErrorPayload:
		return fmt.Sprintf("  %s: %s", p.Error.Code, p.Error.Message)
	}
	return ""
}

// saveAudio decodes the downstream audio per generation and writes
// turn-<seq 3 digits>-gen-<generation>.wav (PCM16, 48 kHz).
func saveAudio(dir string, c *kotokiclient.AudioCollector) ([]string, error) {
	var saved []string
	var errs []error
	for _, g := range c.Generations() {
		dec, err := audio.NewDecoder()
		if err != nil {
			return saved, err
		}
		var pcm []int16
		for _, f := range g.Frames {
			samples, err := dec.Decode(f.Payload)
			if err != nil {
				errs = append(errs, fmt.Errorf("generation %d frame %d: %w", g.Generation, f.Seq, err))
				continue
			}
			pcm = append(pcm, samples...)
		}
		if len(pcm) == 0 {
			continue
		}
		path := filepath.Join(dir, fmt.Sprintf("turn-%03d-gen-%d.wav", g.TurnSeq, g.Generation))
		if err := audio.WriteWAV(path, audio.PCM{Rate: 48000, Samples: pcm}); err != nil {
			errs = append(errs, err)
			continue
		}
		saved = append(saved, path)
	}
	sort.Strings(saved)
	return saved, errors.Join(errs...)
}

// summary is summary.json: the Report plus the session it belongs to.
type summary struct {
	SessionID string `json:"session_id"`
	kotokiclient.Report
}

func writeSummary(dir string, id uuid.UUID, rep kotokiclient.Report) error {
	b, err := json.MarshalIndent(summary{SessionID: id.String(), Report: rep}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "summary.json"), append(b, '\n'), 0o600)
}

// percentile is the nearest-rank percentile of v (0 for none).
func percentile(v []int64, q float64) int64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]int64(nil), v...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	rank := int(q*float64(len(s)) + 0.999999999)
	rank = max(1, min(rank, len(s)))
	return s[rank-1]
}

func stat(v []int64) string {
	if len(v) == 0 {
		return "-"
	}
	return fmt.Sprintf("p50=%d p95=%d", percentile(v, 0.50), percentile(v, 0.95))
}

func (a *app) printSummary(id uuid.UUID, rep kotokiclient.Report, dir string, wavs []string) {
	if a.g.JSON {
		_ = a.printJSON(struct {
			Kind   string   `json:"kind"`
			OutDir string   `json:"out_dir"`
			Audio  []string `json:"audio_files"`
			summary
		}{"summary", dir, wavs, summary{SessionID: id.String(), Report: rep}})
		return
	}
	a.printf("--- summary ---\n")
	a.printf("turns=%d  turn_latency_ms %s  barge_in_stop_ms %s  resumes=%d\n",
		len(rep.TurnLatencyMS), stat(rep.TurnLatencyMS), stat(rep.BargeInStopMS), len(rep.ResumeMS))
	a.printf("connect_ms=%d start_ms=%d\n", rep.ConnectMS, rep.StartMS)
	for _, sl := range rep.ServerLatencies {
		a.printf("server turn %d: turn_end_to_first_audio_ms=%d stt_final_ms=%d llm_first_token_ms=%d tts_first_audio_ms=%d\n",
			sl.TurnSeq, sl.TurnEndToFirstAudioMS, sl.STTFinalMS, sl.LLMFirstTokenMS, sl.TTSFirstAudioMS)
	}
	a.printf("session %s  charged_seconds=%d\n", rep.Status, rep.ChargedSeconds)
	a.printf("output: %s (%d audio files)\n", dir, len(wavs))
}
