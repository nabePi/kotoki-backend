package main

import (
	"context"
	"time"
)

// shutdownSlack is how far the shutdown context outlives the grace. Consumer.Shutdown cancels the
// handlers when the grace ends and must still be able to wait for their nacks; the Kubernetes
// terminationGracePeriodSeconds is grace + 10 s (spec 9.7.7), so 5 s of slack stays inside it.
const shutdownSlack = 5 * time.Second

// Lifecycle is the ordered shutdown of the worker (spec 9.7.7). A nil step is skipped.
type Lifecycle struct {
	// SetNotReady turns /readyz to 503 (step 1).
	SetNotReady func()
	// StopScheduler stops starting jobs and waits for the runs in progress until ctx ends (step 2).
	StopScheduler func(ctx context.Context)
	// ShutdownConsumers is basic.cancel on every consumer, a wait of up to grace for the in-flight
	// handlers, then the cancel of their root context (steps 3 to 5).
	ShutdownConsumers func(ctx context.Context, grace time.Duration) error
	// StopPublisher lets the outbox batch in flight finish and stops the publisher (step 6).
	StopPublisher func(ctx context.Context) error
	// CloseAMQP closes the channels and both connections, ClosePool the pgxpool, FlushOTel the
	// exporter (step 7).
	CloseAMQP func() error
	ClosePool func()
	FlushOTel func(ctx context.Context) error
}

// Shutdown runs the steps in order. Scheduler and consumers share one window of grace: what the
// scheduler spent waiting for its jobs is taken from the grace of the handlers, so the whole
// sequence ends within grace + shutdownSlack of the signal. A failing step never skips the next
// ones; the first error is returned.
func (l Lifecycle) Shutdown(ctx context.Context, grace time.Duration) error {
	deadline := time.Now().Add(grace)
	sctx, cancel := context.WithDeadline(ctx, deadline.Add(shutdownSlack))
	defer cancel()

	var first error
	keep := func(err error) {
		if err != nil && first == nil {
			first = err
		}
	}
	if l.SetNotReady != nil {
		l.SetNotReady()
	}
	if l.StopScheduler != nil {
		l.StopScheduler(sctx)
	}
	if l.ShutdownConsumers != nil {
		keep(l.ShutdownConsumers(sctx, max(time.Until(deadline), 0)))
	}
	if l.StopPublisher != nil {
		keep(l.StopPublisher(sctx))
	}
	if l.CloseAMQP != nil {
		keep(l.CloseAMQP())
	}
	if l.ClosePool != nil {
		l.ClosePool()
	}
	if l.FlushOTel != nil {
		keep(l.FlushOTel(sctx))
	}
	return first
}
