//go:build !windows

// Package vu runs virtual users against the backend: each VU logs in, then plays one planned
// session after the other (spec 13.12.2) and reports what it measured to an Aggregator.
package vu

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/cookiejar"
	"sync"
	"time"

	"github.com/nabePi/kotoki-backend/internal/kotokiclient"
	"github.com/nabePi/kotoki-backend/test/load/scenario"
)

// Credential is one load user (users.csv).
type Credential struct{ Email, Password string }

// Conversation plays one scripted conversation; kotokiclient.RunConversation in production.
type Conversation func(ctx context.Context, c *kotokiclient.Client, s kotokiclient.SessionSpec, steps []kotokiclient.Step, sink kotokiclient.Sink) (kotokiclient.Report, error)

// Runner drives the VUs of one load instance.
type Runner struct {
	Users []Credential
	// VUs is the number of virtual users, at most len(Users); 0 means one per user.
	VUs     int
	Planner *scenario.Planner // the prototype: VU i plays Planner.ForVU(i)
	Conv    Conversation
	Agg     *Aggregator
	// Ramp spreads the VU starts evenly; Hold is how long the run lasts after the ramp (0: until
	// the context is done).
	Ramp, Hold time.Duration
	// BackgroundRESTEvery is the period of each VU's background REST calls (balance and catalog:
	// the client has no history/profile calls, a decision of the plan); 0 turns them off.
	BackgroundRESTEvery time.Duration

	APIURL, WSURL, Origin string
	// Pre holds the pre-encoded packets by fixture name (Plan.Steps).
	Pre map[string][][]byte

	// Optional hooks. NewClient builds a VU's client (default: one cookie jar per VU over a shared
	// transport). Sleep and Now default to the real ones; OnVUStart is called when a VU is launched.
	NewClient func() *kotokiclient.Client
	Sleep     func(ctx context.Context, d time.Duration) error
	Now       func() time.Time
	OnVUStart func(idx int, at time.Time)
	// Backoff is the pause after a failed login, session creation or conversation (default 1 s).
	Backoff time.Duration
	// Logf receives one line per problem (default: dropped).
	Logf func(format string, args ...any)
}

func (r *Runner) sleep(ctx context.Context, d time.Duration) error {
	if r.Sleep != nil {
		return r.Sleep(ctx, d)
	}
	return kotokiclient.SleepContext(ctx, d)
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Runner) backoff() time.Duration {
	if r.Backoff > 0 {
		return r.Backoff
	}
	return time.Second
}

func (r *Runner) logf(format string, args ...any) {
	if r.Logf != nil {
		r.Logf(format, args...)
	}
}

func (r *Runner) vus() int {
	if r.VUs > 0 {
		return r.VUs
	}
	return len(r.Users)
}

// Run starts the VUs spread over Ramp, lets them run for Hold, then cancels them and waits until
// every goroutine is gone. It returns an error only for a bad setup; the end of the run (Hold over
// or ctx done) is not an error. A conversation cut by that end is not recorded.
func (r *Runner) Run(ctx context.Context) error {
	switch {
	case len(r.Users) == 0:
		return errors.New("vu: no users")
	case r.vus() > len(r.Users):
		return fmt.Errorf("vu: %d VUs need as many users, the users file has %d", r.vus(), len(r.Users))
	case r.Planner == nil || r.Conv == nil || r.Agg == nil:
		return errors.New("vu: Planner, Conv and Agg are required")
	}
	n := r.vus()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	newClient := r.NewClient
	if newClient == nil {
		newClient = r.defaultClients()
	}

	var wg sync.WaitGroup
	step := r.Ramp / time.Duration(n)
	for k := 0; k < n; k++ {
		if k > 0 && step > 0 && r.sleep(ctx, step) != nil {
			break
		}
		if r.OnVUStart != nil {
			r.OnVUStart(k, r.now())
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.vu(ctx, k, newClient())
		}()
	}
	if ctx.Err() == nil && step > 0 {
		_ = r.sleep(ctx, step) // the last slot of the ramp
	}
	if ctx.Err() == nil {
		if r.Hold > 0 {
			_ = r.sleep(ctx, r.Hold)
		} else {
			<-ctx.Done()
		}
	}
	cancel()
	wg.Wait()
	return nil
}

// defaultClients gives every VU its own cookie jar (its own refresh cookie) over one shared
// transport, so that connections are reused across VUs.
func (r *Runner) defaultClients() func() *kotokiclient.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConnsPerHost = 256
	return func() *kotokiclient.Client {
		jar, _ := cookiejar.New(nil) // never fails with nil options
		return kotokiclient.New(r.APIURL, kotokiclient.WithHTTPClient(&http.Client{Jar: jar, Timeout: 30 * time.Second, Transport: tr}))
	}
}

// vuState is what one VU keeps between sessions.
type vuState struct {
	r       *Runner
	idx     int
	cred    Credential
	c       *kotokiclient.Client
	rng     *rand.Rand
	catalog kotokiclient.Catalog
}

func (r *Runner) vu(ctx context.Context, idx int, c *kotokiclient.Client) {
	//nolint:gosec // G404: picks of a load model, not a security decision
	v := &vuState{r: r, idx: idx, cred: r.Users[idx], c: c, rng: rand.New(rand.NewPCG(uint64(idx), 0x6c6f6164))}
	pl := r.Planner.ForVU(idx)
	if !v.setup(ctx) {
		return
	}
	var bg sync.WaitGroup
	if r.BackgroundRESTEvery > 0 {
		bg.Add(1)
		go func() { defer bg.Done(); v.background(ctx) }()
	}
	defer bg.Wait()
	for ctx.Err() == nil {
		if err := v.session(ctx, pl.Next()); err != nil && ctx.Err() == nil {
			r.logf("vu %d: %v", idx, err)
			_ = r.sleep(ctx, r.backoff())
		}
	}
}

// setup logs in and loads the catalog, retrying with the backoff until it works or the run ends.
func (v *vuState) setup(ctx context.Context) bool {
	r := v.r
	for ctx.Err() == nil {
		err := v.c.Login(ctx, v.cred.Email, v.cred.Password)
		if err == nil {
			var cat kotokiclient.Catalog
			if cat, err = v.c.Catalog(ctx); err == nil {
				if len(cat.Scenarios) == 0 || len(cat.Characters) == 0 {
					err = errors.New("the catalog has no scenario or character")
				} else {
					v.catalog = cat
					return true
				}
			}
		}
		if ctx.Err() != nil {
			return false
		}
		r.Agg.addLoginError()
		r.logf("vu %d: setup: %v", v.idx, err)
		_ = r.sleep(ctx, r.backoff())
	}
	return false
}

// authed runs f and, when the access token has expired (401), signs in again once and retries.
func (v *vuState) authed(ctx context.Context, f func() error) error {
	err := f()
	var api *kotokiclient.APIError
	if errors.As(err, &api) && api.Status == http.StatusUnauthorized {
		if lerr := v.c.Login(ctx, v.cred.Email, v.cred.Password); lerr != nil {
			return fmt.Errorf("login after 401: %w", lerr)
		}
		return f()
	}
	return err
}

func (v *vuState) session(ctx context.Context, plan scenario.SessionPlan) error {
	r := v.r
	sc := v.catalog.Scenarios[v.rng.IntN(len(v.catalog.Scenarios))]
	ch := v.catalog.Characters[v.rng.IntN(len(v.catalog.Characters))]
	var sess kotokiclient.Session
	err := v.authed(ctx, func() (e error) {
		sess, e = v.c.CreateSession(ctx, kotokiclient.CreateSessionRequest{
			TopicID: sc.TopicID, ScenarioID: sc.ID, CharacterID: ch.ID,
			DurationSeconds: plan.DurationSeconds, ConversationStartMode: plan.StartMode,
		})
		return e
	})
	if err != nil {
		r.Agg.addCreateError()
		return fmt.Errorf("create session: %w", err)
	}
	rep, err := r.Conv(ctx, v.c, kotokiclient.SessionSpec{SessionID: sess.ID, WSURL: r.WSURL, Origin: r.Origin},
		plan.Steps(r.Pre), kotokiclient.NopSink{})
	if ctx.Err() != nil {
		return nil // cut by the end of the run: not a result
	}
	drops := 0
	if plan.Drop != nil {
		drops = 1
	}
	r.Agg.Add(samplesFromReport(rep), drops, err)
	if err != nil {
		return fmt.Errorf("session %s: %w", sess.ID, err)
	}
	return nil
}

// background calls the REST endpoints a user's app would poll (a decision of the plan: the client
// has no history/profile calls, so balance and catalog stand in).
func (v *vuState) background(ctx context.Context) {
	r := v.r
	for r.sleep(ctx, r.BackgroundRESTEvery) == nil {
		err := v.authed(ctx, func() error { _, e := v.c.Balance(ctx); return e })
		r.Agg.addREST(err != nil && ctx.Err() == nil)
		err = v.authed(ctx, func() error { _, e := v.c.Catalog(ctx); return e })
		r.Agg.addREST(err != nil && ctx.Err() == nil)
	}
}

// samplesFromReport is the only place that reads the fields of a kotokiclient.Report; adjust it
// here when M5 renames them.
func samplesFromReport(rep kotokiclient.Report) Samples {
	return Samples{ConnectMS: rep.ConnectMS, TurnLatencyMS: rep.TurnLatencyMS, BargeInStopMS: rep.BargeInStopMS, ResumeMS: rep.ResumeMS}
}
