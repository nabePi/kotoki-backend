//go:build !windows

package vu

import (
	"sync"

	"github.com/nabePi/kotoki-backend/test/load/hist"
	"github.com/nabePi/kotoki-backend/test/load/server"
)

// Samples are the client-side measurements of one conversation, in milliseconds, as the one
// adaptor (samplesFromReport) reads them from a kotokiclient.Report.
type Samples struct {
	ConnectMS     int64
	TurnLatencyMS []int64
	BargeInStopMS []int64
	ResumeMS      []int64
}

// Aggregator collects what the VUs measure. It is safe for concurrent use.
type Aggregator struct {
	mu sync.Mutex

	turn, bargeIn, resume, connect *hist.Histogram

	connectOK, connectTotal int
	resumeOK, resumeTotal   int
	sessions, sessionErrors int
	loginErrors             int
	createErrors            int
	restRequests, restErrs  int
	outboxLag               []float64
}

// NewAggregator returns an empty Aggregator.
func NewAggregator() *Aggregator {
	return &Aggregator{turn: hist.New(), bargeIn: hist.New(), resume: hist.New(), connect: hist.New()}
}

// Add records one finished conversation. expectedResumes is how many drops its plan held: each one
// that did not come back (fewer ResumeMS than drops) counts as a failed resume. The conversation
// counts as connected when the WebSocket was up at some point (ConnectMS > 0) or it ended without
// error.
func (a *Aggregator) Add(s Samples, expectedResumes int, convErr error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sessions++
	if convErr != nil {
		a.sessionErrors++
	}
	a.connectTotal++
	if s.ConnectMS > 0 || convErr == nil {
		a.connectOK++
	}
	if s.ConnectMS > 0 {
		a.connect.Record(seconds(s.ConnectMS))
	}
	for _, v := range s.TurnLatencyMS {
		a.turn.Record(seconds(v))
	}
	for _, v := range s.BargeInStopMS {
		a.bargeIn.Record(seconds(v))
	}
	for _, v := range s.ResumeMS {
		a.resume.Record(seconds(v))
	}
	a.resumeTotal += expectedResumes
	a.resumeOK += min(len(s.ResumeMS), expectedResumes)
}

// AddOutboxLag records one periodic sample of the outbox lag, in seconds.
func (a *Aggregator) AddOutboxLag(v float64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.outboxLag = append(a.outboxLag, v)
}

func (a *Aggregator) addLoginError()  { a.mu.Lock(); a.loginErrors++; a.mu.Unlock() }
func (a *Aggregator) addCreateError() { a.mu.Lock(); a.createErrors++; a.mu.Unlock() }
func (a *Aggregator) addREST(failed bool) {
	a.mu.Lock()
	a.restRequests++
	if failed {
		a.restErrs++
	}
	a.mu.Unlock()
}

// Snapshot returns a copy of the aggregate in the shape the server-side evaluation takes.
func (a *Aggregator) Snapshot() server.ClientAgg {
	a.mu.Lock()
	defer a.mu.Unlock()
	return server.ClientAgg{
		ClientBargeIn: cloneHist(a.bargeIn), Resume: cloneHist(a.resume),
		ConnectOK: a.connectOK, ConnectTotal: a.connectTotal,
		ResumeOK: a.resumeOK, ResumeTotal: a.resumeTotal,
		OutboxLagSamples: append([]float64(nil), a.outboxLag...),
	}
}

// Summary is the client-side view of a run, written to the result JSON.
type Summary struct {
	Sessions      int     `json:"sessions"`
	SessionErrors int     `json:"session_errors"`
	LoginErrors   int     `json:"login_errors"`
	CreateErrors  int     `json:"create_errors"`
	RESTRequests  int     `json:"rest_requests"`
	RESTErrors    int     `json:"rest_errors"`
	ConnectP95S   float64 `json:"connect_p95_s"`
	TurnP95S      float64 `json:"turn_p95_s"`
	BargeInP95S   float64 `json:"bargein_p95_s"`
	ResumeP95S    float64 `json:"resume_p95_s"`
}

// Summary returns the counters and the client-side P95s (0 where nothing was measured: the file is
// a report, the gate reads the Observation).
func (a *Aggregator) Summary() Summary {
	a.mu.Lock()
	defer a.mu.Unlock()
	return Summary{
		Sessions: a.sessions, SessionErrors: a.sessionErrors, LoginErrors: a.loginErrors,
		CreateErrors: a.createErrors, RESTRequests: a.restRequests, RESTErrors: a.restErrs,
		ConnectP95S: p95(a.connect), TurnP95S: p95(a.turn), BargeInP95S: p95(a.bargeIn), ResumeP95S: p95(a.resume),
	}
}

func seconds(ms int64) float64 { return float64(ms) / 1000 }

func cloneHist(h *hist.Histogram) *hist.Histogram {
	c := hist.New()
	c.Merge(h)
	return c
}

func p95(h *hist.Histogram) float64 {
	if h.Count() == 0 {
		return 0
	}
	return h.Quantile(0.95)
}
