package conversation

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nabePi/kotoki-backend/internal/conversation/audio"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/session"
)

// Constants of the upstream audio path (spec §7.5).
const (
	maxPLCGap          = 5                      // gap ≤ 5: packet loss concealment per lost frame
	maxSilenceGap      = 50                     // gap ≤ 50: that many silence frames; more: 50
	invalidNoticeEvery = 5 * time.Second        // AUDIO_FRAME_INVALID is sent at most once per 5 s
	starvationAfter    = 200 * time.Millisecond // no upstream frame for this long while USER_SPEAKING: inject silence
)

// frameEvent is what the audio pipeline reports to the loop (frameEvents, cap 64): one 20 ms frame
// with its VAD verdict (the frames the pipeline inserted for a gap or an undecodable packet
// included), or the barrier marker of Runtime.Barrier.
type frameEvent struct {
	barrier chan struct{}
	voiced  bool
	// invalid asks the loop for an AUDIO_FRAME_INVALID error event (already rate limited to one
	// per 5 s by the pipeline).
	invalid bool
	at      time.Time // when the reader received the frame
}

// sttBox lets the audio goroutine read the STT stream the loop owns without a lock: the loop
// stores the stream when the session is active (Task 21 stores nil when the stream is closed).
type sttBox struct{ s provider.RoutedSTTSession }

// audioPipeline is the audioPipeline goroutine of one connection (§7.5, §7.7.2): it takes the
// opus packets of audioIn, applies the seq rules (duplicate, gap, PLC), decodes straight to 16 kHz
// mono, runs the VAD, sends the PCM to the STT stream (always, also while the AI speaks, D-080)
// and tells the loop about every frame. It owns the decoder, the VAD and `expected`; nothing else
// touches them.
type audioPipeline struct {
	r        *Runtime
	cs       *connState
	dec      *audio.Decoder
	vad      *audio.VAD
	expected uint32
	invalid  *burstWindow
	lastNote time.Time // the last AUDIO_FRAME_INVALID notice
	pcm      []byte    // reused PCM16LE buffer, 640 bytes
	silence  []int16
}

// startAudio runs the audio side of a connection: the pipeline consumes cs.audioIn and answers
// the Runtime.Barrier markers in order with the frames.
func (r *Runtime) startAudio(cs *connState) {
	cs.goSafe("audio", func() error {
		p, err := r.newAudioPipeline(cs)
		if err != nil {
			return &panicError{who: "audio pipeline", value: err}
		}
		return p.run()
	})
}

func (r *Runtime) newAudioPipeline(cs *connState) (*audioPipeline, error) {
	dec, err := audio.NewDecoder()
	if err != nil {
		return nil, fmt.Errorf("conversation: audio decoder: %w", err)
	}
	vad, err := audio.NewVAD(r.d.Cfg.VADMode)
	if err != nil {
		return nil, fmt.Errorf("conversation: vad: %w", err)
	}
	return &audioPipeline{
		r: r, cs: cs, dec: dec, vad: vad,
		invalid: newBurstWindow(r.d.Clock, audioFrameInvalidLimit, audioFrameInvalidWindow),
		pcm:     make([]byte, 0, 2*audio.UpstreamFrame),
		silence: make([]int16, audio.UpstreamFrame),
	}, nil
}

// run consumes audioIn until the connection ends; a *ProtocolError (more than 50 undecodable
// frames in 10 s) ends it as a violation the loop answers.
func (p *audioPipeline) run() error {
	ctx := p.cs.ctx
	for {
		select {
		case <-ctx.Done():
			return nil
		case a := <-p.cs.audioIn:
			if a.Barrier != nil {
				select {
				case p.r.frameEvents <- frameEvent{barrier: a.Barrier}:
				case <-ctx.Done():
					return nil
				}
				continue
			}
			ok, err := p.packet(ctx, a)
			if err != nil {
				return err
			}
			if !ok {
				return nil
			}
		}
	}
}

// packet applies §7.5.2 to one upstream packet; false when the connection ended.
func (p *audioPipeline) packet(ctx context.Context, a AudioIn) (bool, error) {
	m := p.r.d.Metrics
	switch {
	case a.Seq == p.expected:
		p.expected++
		m.audioIn("ok")
	case a.Seq < p.expected:
		m.audioIn("dup") // a duplicate or late frame: dropped
		return true, nil
	default:
		gap := int(a.Seq - p.expected)
		p.fill(ctx, gap, a.At)
		p.expected = a.Seq + 1
		m.audioIn("gap")
	}
	t0 := p.r.d.Clock.Now()
	pcm, err := p.dec.Decode(a.Opus)
	m.FrameProcessing("opus_decode", p.r.d.Clock.Now().Sub(t0))
	if err != nil { // §7.5.1: dropped, and a silence frame keeps the audio timeline moving
		m.audioIn("invalid")
		if p.invalid.Add() {
			m.protocolViolation(ViolationInvalidBurst)
			return false, &ProtocolError{Code: errs.CodeProtocolViolation, Kind: ViolationInvalidBurst, Detail: "more than 50 AUDIO_FRAME_INVALID in 10 s"}
		}
		note := p.lastNote.IsZero() || a.At.Sub(p.lastNote) >= invalidNoticeEvery
		if note {
			p.lastNote = a.At
		}
		return p.frame(ctx, p.silence, a.At, note), nil
	}
	return p.frame(ctx, pcm, a.At, false), nil
}

// fill inserts the frames of a gap before the frame that revealed it (§7.5.2): PLC for up to 5
// lost frames, silence for up to 50, and 50 silence frames beyond that.
func (p *audioPipeline) fill(ctx context.Context, gap int, at time.Time) {
	switch {
	case gap <= maxPLCGap:
		for range gap {
			pcm, err := p.dec.DecodePLC()
			if err != nil {
				pcm = p.silence // PLC failed: silence
			}
			if !p.frame(ctx, pcm, at, false) {
				return
			}
		}
	case gap <= maxSilenceGap:
		for range gap {
			if !p.frame(ctx, p.silence, at, false) {
				return
			}
		}
	default:
		p.r.log.Warn("upstream discontinuity", "gap_frames", gap)
		for range maxSilenceGap {
			if !p.frame(ctx, p.silence, at, false) {
				return
			}
		}
	}
}

// frame is one 20 ms frame: VAD, STT, then the loop. False when the connection ended.
func (p *audioPipeline) frame(ctx context.Context, pcm []int16, at time.Time, invalid bool) bool {
	clk := p.r.d.Clock
	t0 := clk.Now()
	voiced := p.vad.Process(pcm) // an error counts as silence (§7.5.3)
	p.r.d.Metrics.FrameProcessing("vad", clk.Now().Sub(t0))
	if box := p.r.sttPipe.Load(); box != nil {
		p.pcm = audio.Int16ToBytes(pcm, p.pcm)
		if err := box.s.SendAudio(p.pcm); err != nil && !errors.Is(err, provider.ErrSessionClosed) {
			p.r.log.Warn("stt send audio failed", "error", err.Error())
		}
	}
	select {
	case p.r.frameEvents <- frameEvent{voiced: voiced, invalid: invalid, at: at}:
		return true
	case <-ctx.Done():
		return false
	}
}

// ---- loop side ----

// onFrameEvent feeds one frame to the detector (§7.6.2). Frames before the session is active are
// dropped: the detector has no turn to serve yet.
func (r *Runtime) onFrameEvent(fe frameEvent) {
	if fe.barrier != nil { // a Barrier marker is not input: it must not move Handled
		close(fe.barrier)
		return
	}
	r.handled++
	if r.status != session.StatusActive && r.status != session.StatusEnding {
		return
	}
	if fe.invalid {
		r.emitError(errs.CodeAudioFrameInvalid, nil, nil)
	}
	r.lastFrameAt = fe.at
	r.pushFrame(fe.voiced)
}

// pushFrame runs one frame through the detector and acts on what it reports. The audio clock of
// the session (sttState's audioNow) is the detector's.
func (r *Runtime) pushFrame(voiced bool) {
	if voiced && r.turn != nil && r.turnState == StateUserSpeaking {
		r.turn.voicedMs += FrameMs
	}
	evs := r.det.Push(voiced, r.turnState, r.sttSt)
	r.audioMs = r.det.NowMs()
	for _, ev := range evs {
		r.onDetector(ev)
		if r.stopped {
			return
		}
	}
}

// checkStarvation is the watchdog of §7.5.2: with no upstream frame for more than 200 ms while the
// user is speaking (a muted or stalled client), the loop injects the missing silence so the turn
// can still end. Detector time is audio time, so the injected ms are those that passed.
func (r *Runtime) checkStarvation(now time.Time) {
	if !r.connected || r.status != session.StatusActive || r.turnState != StateUserSpeaking || r.lastFrameAt.IsZero() {
		return
	}
	el := now.Sub(r.lastFrameAt)
	if el <= starvationAfter {
		return
	}
	ms := int64(el/time.Millisecond) / FrameMs * FrameMs
	r.lastFrameAt = r.lastFrameAt.Add(time.Duration(ms) * time.Millisecond)
	for range ms / FrameMs {
		r.pushFrame(false)
		if r.stopped || r.turnState != StateUserSpeaking {
			return
		}
	}
}
