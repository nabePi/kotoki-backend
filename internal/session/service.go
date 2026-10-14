package session

import (
	"context"
	"log/slog"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/balance"
	"github.com/nabePi/kotoki-backend/internal/content"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
)

// Metrics receives the session counters; *metrics.Session implements it.
type Metrics interface {
	Created(result string) // ok | insufficient_balance | error
	Ended(service, endReason, status string)
}

// NopMetrics ignores everything.
type NopMetrics struct{}

// Created implements Metrics.
func (NopMetrics) Created(string) {}

// Ended implements Metrics.
func (NopMetrics) Ended(string, string, string) {}

// Deps are the collaborators of Service, wired by hand in the composition root.
type Deps struct {
	Store          Store
	Reader         Repository // reads outside a transaction (the backstop of Create, REST queries)
	Balance        *balance.Service
	Resolver       content.Resolver
	Adaptive       AdaptiveProvider
	Users          UserGate
	OpeningPrompts OpeningPromptSource
	Control        ControlPublisher
	Clock          clock.Clock
	IDs            idgen.Generator
	Log            *slog.Logger
	TermsVersion   string
	// Producer names the event producer of a terminal transition: REALTIME when the runtime closes
	// the session (fence non-nil), WORKER when the sweeper does. Nil uses exactly that rule.
	Producer    func(Fence) string
	Metrics     Metrics
	ServiceName string // api | realtime | worker: the "service" label of kotoki_sessions_ended_total
}

// Service is the single writer of sessions.status (spec 5.2). Its methods are spread over
// create.go (creation), release.go (cancel, abandon, start failure), runtime.go-facing
// transitions and settle.go (ending and settlement).
type Service struct {
	d Deps
}

// NewService builds the service; Log, Metrics and Producer default to harmless values.
func NewService(d Deps) *Service {
	if d.Log == nil {
		d.Log = slog.New(slog.DiscardHandler)
	}
	if d.Metrics == nil {
		d.Metrics = NopMetrics{}
	}
	if d.Producer == nil {
		d.Producer = defaultProducer
	}
	if d.Control == nil {
		d.Control = noControl{}
	}
	return &Service{d: d}
}

func defaultProducer(f Fence) string {
	if f != nil {
		return "REALTIME"
	}
	return "WORKER"
}

type noControl struct{}

func (noControl) Publish(context.Context, uuid.UUID, ControlMessage) error { return nil }
