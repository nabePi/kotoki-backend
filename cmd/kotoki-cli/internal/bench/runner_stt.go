package bench

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/provider"
)

const (
	sttFrameBytes  = provider.STTSampleRate * 2 * 20 / 1000 // one 20 ms frame
	sttSendRetries = 400                                    // x5 ms: how long a full vendor buffer may block unpaced sending
	sttSendBackoff = 5 * time.Millisecond
	sttCloseGrace  = time.Second
	pcmBytesPerMs  = provider.STTSampleRate * 2 / 1000
)

// sttOutcome is what one STT attempt measured.
type sttOutcome struct {
	text           string
	connectMs      float64
	finalMs        float64 // from Finalize() to the FINAL it asked for
	firstPartialMs float64 // from the first audio sent; valid when hasPartial
	hasPartial     bool
	partials       int
	revisions      int // partials that rewrote the text instead of extending it
}

// sttCollector reads the session events on its own goroutine, so that the first partial is stamped
// when it arrives and not when the sender gets round to it.
type sttCollector struct {
	clk  clock.Clock
	mu   sync.Mutex
	done chan struct{}
	once sync.Once

	start        time.Time
	texts        []string
	lastPartial  string
	partials     int
	revisions    int
	firstPartial time.Time
	finalAt      time.Time
	err          *provider.Error
}

func (c *sttCollector) finish() { c.once.Do(func() { close(c.done) }) }

func (c *sttCollector) run(events <-chan provider.STTEvent, exited chan<- struct{}) {
	defer close(exited)
	for ev := range events {
		now := c.clk.Now()
		c.mu.Lock()
		switch ev.Type {
		case provider.STTPartial:
			c.partials++
			if c.firstPartial.IsZero() {
				c.firstPartial = now
			}
			if c.lastPartial != "" && !strings.HasPrefix(ev.Text, c.lastPartial) {
				c.revisions++
			}
			c.lastPartial = ev.Text
		case provider.STTFinal:
			c.texts = append(c.texts, ev.Text)
			c.lastPartial = ""
			if ev.FromFinalize {
				c.finalAt = now
				c.mu.Unlock()
				c.finish()
				continue
			}
		case provider.STTError:
			c.err = ev.Err
			if c.err == nil {
				c.err = provider.NewError("", provider.CapSTT, provider.KindUnavailable, "stt error event without detail")
			}
			c.mu.Unlock()
			c.finish()
			continue
		case provider.STTClosed:
			if c.finalAt.IsZero() && c.err == nil {
				c.err = provider.NewError("", provider.CapSTT, provider.KindUnavailable, "stt session closed before the final")
			}
			c.mu.Unlock()
			c.finish()
			continue
		}
		c.mu.Unlock()
	}
}

// sttOnce runs one utterance through a fresh session: open, send the PCM (paced or not), Finalize,
// wait for the FINAL of that Finalize.
func sttOnce(ctx context.Context, p provider.STTProvider, pcm []byte, clk clock.Clock, o runOpts) (sttOutcome, error) {
	ctx, cancel := context.WithTimeout(ctx, o.timeout)
	defer cancel()
	t0 := clk.Now()
	sess, err := p.Open(ctx, provider.STTSessionConfig{Language: "ja", SampleRate: provider.STTSampleRate})
	if err != nil {
		return sttOutcome{}, err
	}
	out := sttOutcome{connectMs: ms(clk.Now().Sub(t0))}
	col := &sttCollector{clk: clk, done: make(chan struct{})}
	exited := make(chan struct{})
	go col.run(sess.Events(), exited)
	defer func() {
		_ = sess.Close()
		select {
		case <-exited:
		case <-time.After(sttCloseGrace):
		}
	}()

	col.mu.Lock()
	col.start = clk.Now()
	col.mu.Unlock()
	for off := 0; off < len(pcm); off += sttFrameBytes {
		frame := pcm[off:min(off+sttFrameBytes, len(pcm))]
		if len(frame)%2 != 0 {
			frame = frame[:len(frame)-1]
		}
		if err := sendFrame(ctx, sess, frame); err != nil {
			return out, err
		}
		if o.pacing {
			if err := sleepCtx(ctx, 20*time.Millisecond); err != nil {
				return out, err
			}
		}
	}
	tFin := clk.Now()
	if err := sess.Finalize(); err != nil {
		return out, err
	}
	select {
	case <-col.done:
	case <-ctx.Done():
		return out, ctx.Err()
	}
	col.mu.Lock()
	defer col.mu.Unlock()
	if col.err != nil {
		return out, col.err
	}
	out.finalMs = ms(col.finalAt.Sub(tFin))
	out.text = strings.Join(col.texts, "")
	out.partials, out.revisions = col.partials, col.revisions
	if !col.firstPartial.IsZero() {
		out.hasPartial, out.firstPartialMs = true, ms(col.firstPartial.Sub(col.start))
	}
	return out, nil
}

// sendFrame sends one frame. A vendor buffer that is full (UNAVAILABLE, at most 2 s of audio) is
// waited out for unpaced sending; any other error ends the attempt.
func sendFrame(ctx context.Context, sess provider.STTSession, frame []byte) error {
	for try := 0; ; try++ {
		err := sess.SendAudio(frame)
		if err == nil {
			return nil
		}
		if provider.KindOf(err) != provider.KindUnavailable || try >= sttSendRetries {
			return err
		}
		if err := sleepCtx(ctx, sttSendBackoff); err != nil {
			return err
		}
	}
}

// RunSTT measures the STT candidate on every item, `repeat` times each, one session per attempt:
// connect time, first partial (if the vendor streams one before the commit), final after Finalize,
// partial revisions and the CER of the concatenated FINAL texts against the reference. A vendor error
// becomes an error row, not an abort; only ctx ends the run early. Audio comes from audio; clk
// stamps the events (clock.System in real runs).
func RunSTT(ctx context.Context, p provider.STTProvider, c Candidate, items []Item, audio AudioSource, repeat int, clk clock.Clock, opts ...Option) []Result {
	o := newRunOpts(opts)
	var rs []Result
	for _, it := range items {
		pcm, aerr := audio.PCM16k(it)
		for n := 1; n <= repeat; n++ {
			if ctx.Err() != nil {
				return rs
			}
			r := newResult(SuiteSTT, c, it, n)
			if aerr != nil {
				r.Values, r.Flags, r.ErrorKind = nil, nil, "AUDIO"
				rs = append(rs, r)
				continue
			}
			out, err := sttOnce(ctx, p, pcm, clk, o)
			if err != nil {
				rs = append(rs, r.failed(err))
				continue
			}
			edits, refLen := Edits(it.ReferenceText, out.text)
			r.Values["connect_ms"] = out.connectMs
			r.Values["final_after_finalize_ms"] = out.finalMs
			r.Values["partials"] = float64(out.partials)
			r.Values["partial_revisions"] = float64(out.revisions)
			r.Values["audio_ms"] = float64(len(pcm) / pcmBytesPerMs)
			r.Values["cer_edits"] = float64(edits)
			r.Values["cer_ref_len"] = float64(refLen)
			r.Values["cer"] = CER(it.ReferenceText, out.text)
			r.Flags["has_partial"] = out.hasPartial
			if out.hasPartial {
				r.Values["first_partial_ms"] = out.firstPartialMs
			}
			rs = append(rs, r)
		}
	}
	return rs
}
