package conversation

import (
	"context"
	"sync"
	"time"

	"github.com/nabePi/kotoki-backend/internal/conversation/audio"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
)

const (
	pacerFrameMS      = 20 // every OutFrame is 20 ms of audio
	pacerFrameStep    = pacerFrameMS * time.Millisecond
	pacerMaxQueued    = 25 // network backpressure: Outbound audio frames (§7.7.2)
	pacerDefaultSleep = pacerFrameStep
)

// PacerHooks are optional callbacks of Pacer.Run, all called on the pacer goroutine.
// Order for the first frame: BeforeFirst, EnqueueAudio, OnFirst, OnSentenceStart.
type PacerHooks struct {
	// BeforeFirst runs right before the first frame is enqueued ("audio sent", MarkAudioSent).
	BeforeFirst func()
	// OnFirst runs right after the first frame is enqueued.
	OnFirst func()
	// OnSentenceStart runs after the first frame of each sentence is enqueued.
	OnSentenceStart func(f audio.OutFrame)
	// OnDone runs when frames is closed and the end marker is queued (not when cancelled).
	OnDone func(frameCount int, totalMS int64)
}

// Pacer sends one generation's Opus frames downstream in real time while allowing a bounded
// lead over the client's estimated playback position (§7.5.4). One Pacer per generation.
type Pacer struct {
	clk       clock.Clock
	sleep     func(ctx context.Context, d time.Duration) error
	maxLeadMS int64

	mu     sync.Mutex
	start  time.Time
	sentMS int64
}

// NewPacer creates a pacer. sleep waits d or returns ctx.Err() (the seam for a fake clock);
// maxLeadMS is AUDIO_MAX_LEAD_MS (400).
func NewPacer(clk clock.Clock, sleep func(ctx context.Context, d time.Duration) error, maxLeadMS int64) *Pacer {
	return &Pacer{clk: clk, sleep: sleep, maxLeadMS: maxLeadMS}
}

// EstimatedPlaybackEnd is start + sentMs: when the client finishes playing everything sent so
// far (zero before the first frame). Safe for concurrent use.
func (p *Pacer) EstimatedPlaybackEnd() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.start.IsZero() {
		return time.Time{}
	}
	return p.start.Add(time.Duration(p.sentMS) * time.Millisecond)
}

// Run paces frames into out until frames is closed (nil error) or ctx ends (ctx error).
//
// End marker: when frames is closed, ctx is still live and at least one frame was sent, Run
// queues out.EnqueueAudioEnd(gen) after the last frame so the Writer acks AUDIO_WRITTEN_ALL
// (§7.7.4). A cancelled generation (ctx done, e.g. barge-in or expiry) never gets the marker,
// and neither does a generation without audio (nothing was written, so nothing to ack).
func (p *Pacer) Run(ctx context.Context, frames <-chan audio.OutFrame, out *Outbound, hooks PacerHooks) error {
	var (
		count   int
		gen     uint32
		lastSen = -1
	)
	for f := range frames {
		if err := ctx.Err(); err != nil {
			return err
		}
		p.mu.Lock()
		if p.start.IsZero() {
			p.start = p.clk.Now()
		}
		p.mu.Unlock()
		for {
			p.mu.Lock()
			now := p.clk.Now()
			lead := p.sentMS - now.Sub(p.start).Milliseconds()
			if lead < 0 { // underrun (TTS gap): reset the clock so there is no excess burst
				p.start = now.Add(-time.Duration(p.sentMS) * time.Millisecond)
				lead = 0
			}
			p.mu.Unlock()
			if lead < p.maxLeadMS && out.AudioLen() < pacerMaxQueued {
				break
			}
			wait := pacerDefaultSleep // backpressure only: poll every frame time
			if lead >= p.maxLeadMS {
				wait = min(pacerFrameStep, time.Duration(lead-p.maxLeadMS+1)*time.Millisecond)
			}
			if err := p.sleep(ctx, wait); err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		if count == 0 && hooks.BeforeFirst != nil {
			hooks.BeforeFirst()
		}
		out.EnqueueAudio(f)
		gen = f.Gen
		if count == 0 && hooks.OnFirst != nil {
			hooks.OnFirst()
		}
		if f.SentenceIdx != lastSen {
			lastSen = f.SentenceIdx
			if hooks.OnSentenceStart != nil {
				hooks.OnSentenceStart(f)
			}
		}
		count++
		p.mu.Lock()
		p.sentMS += pacerFrameMS
		p.mu.Unlock()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	total := p.sentMS
	p.mu.Unlock()
	if count > 0 {
		out.EnqueueAudioEnd(gen)
	}
	if hooks.OnDone != nil {
		hooks.OnDone(count, total)
	}
	return nil
}
