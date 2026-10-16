package bench

import (
	"context"
	"sync"

	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/provider"
)

// RunConcurrency ramps the load of one candidate: for each step n (in the order given, e.g.
// 10, 50, 100, 200) it starts n calls at the same moment and waits for all of them. One row per step:
// p50_ms and p95_ms of the call latency, rate_limited_pct (calls that failed with RATE_LIMITED, i.e.
// HTTP 429) and error_pct (ALL failed calls, rate-limited ones included), in percent. A step is
// complete even when its calls fail; only ctx ends the ramp early.
func RunConcurrency(ctx context.Context, c Candidate, steps []int, call func(ctx context.Context) error, clk clock.Clock) []Result {
	var rs []Result
	for i, n := range steps {
		if ctx.Err() != nil || n <= 0 {
			return rs
		}
		lat := make([]float64, n)
		kinds := make([]provider.ErrorKind, n)
		failed := make([]bool, n)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for j := 0; j < n; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				t0 := clk.Now()
				err := call(ctx)
				lat[j] = ms(clk.Now().Sub(t0))
				if err != nil {
					failed[j], kinds[j] = true, provider.KindOf(err)
				}
			}()
		}
		close(start)
		wg.Wait()
		var errs, limited int
		for j := range failed {
			if failed[j] {
				errs++
			}
			if kinds[j] == provider.KindRateLimited {
				limited++
			}
		}
		r := newResult(SuiteConcurrency, c, Item{}, i+1)
		r.Values["step"] = float64(n)
		r.Values["p50_ms"] = Percentile(lat, 50)
		r.Values["p95_ms"] = Percentile(lat, 95)
		r.Values["rate_limited_pct"] = 100 * float64(limited) / float64(n)
		r.Values["error_pct"] = 100 * float64(errs) / float64(n)
		rs = append(rs, r)
	}
	return rs
}

// STTCall is the call RunConcurrency makes for an STT candidate: one full utterance (open, send,
// Finalize, wait for the FINAL) of the item. The audio is read once, here.
func STTCall(p provider.STTProvider, it Item, audio AudioSource, clk clock.Clock, opts ...Option) (func(context.Context) error, error) {
	pcm, err := audio.PCM16k(it)
	if err != nil {
		return nil, err
	}
	o := newRunOpts(opts)
	return func(ctx context.Context) error {
		_, err := sttOnce(ctx, p, pcm, clk, o)
		return err
	}, nil
}

// LLMCall is the call RunConcurrency makes for an LLM candidate: the prompt of the item, stopped after
// the first sentence (the runtime hands that sentence to TTS), so a ramp does not pay for full answers.
func LLMCall(p provider.LLMProvider, c Candidate, it Item, clk clock.Clock, opts ...Option) func(context.Context) error {
	o := newRunOpts(opts)
	return func(ctx context.Context) error {
		_, err := llmOnce(ctx, p, provider.LLMRequest{Model: c.Model, Messages: it.Messages, MaxTokens: o.maxTokens, Temperature: o.temperature, RequestID: "bench:conc:" + it.ID}, clk, o.timeout, true)
		return err
	}
}

// TTSCall is the call RunConcurrency makes for a TTS candidate: the sentence of the item.
func TTSCall(p provider.TTSProvider, c Candidate, it Item, clk clock.Clock, opts ...Option) func(context.Context) error {
	o := newRunOpts(opts)
	return func(ctx context.Context) error {
		_, err := ttsOnce(ctx, p, c, it.ReferenceText, "bench:conc:"+it.ID, clk, o.timeout)
		return err
	}
}
