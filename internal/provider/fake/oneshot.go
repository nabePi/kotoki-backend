package fake

import (
	"context"
	"errors"
	"time"

	"github.com/nabePi/kotoki-backend/internal/provider"
)

func ctxErr(name provider.Name, c provider.Capability, err error) *provider.Error {
	if errors.Is(err, context.DeadlineExceeded) {
		e := errorFor(name, c, provider.KindTimeout)
		e.Cause = err
		return e
	}
	return cancelledErr(name, c, err)
}

// oneShot applies fault injection and latency to a non-streaming call (evaluation,
// embedding). It returns the failure to report, or nil to proceed with the normal result;
// the planned latency is returned for Latency fields.
func oneShot(ctx context.Context, d Deps, jit *jitterSource, dec *decider, cfg CapConfig, name provider.Name, c provider.Capability) (time.Duration, *provider.Error) {
	if err := ctx.Err(); err != nil {
		return 0, ctxErr(name, c, err)
	}
	fault, _ := d.Faults.Current().Lookup(c, name)
	fail, plan := dec.next(fault)
	if fail && plan.Phase == PhaseStart {
		return 0, errorFor(name, c, plan.Kind)
	}
	if fail && plan.Phase == PhaseHang {
		<-ctx.Done()
		return 0, ctxErr(name, c, ctx.Err())
	}
	wait := jit.delay(cfg.LatencyMS, cfg.JitterMS) + time.Duration(plan.ExtraDelayMs)*time.Millisecond
	if !sleep(ctx, wait) {
		return wait, ctxErr(name, c, ctx.Err())
	}
	if fail { // before_first and mid both fail a non-streaming call after its latency
		return wait, errorFor(name, c, plan.Kind)
	}
	return wait, nil
}
