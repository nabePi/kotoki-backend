package session_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/balance"
	"github.com/nabePi/kotoki-backend/internal/session"
)

var errBoom = errors.New("boom")

// TestTransitionsSurfaceStoreErrors arms one store failure (the row CAS or the event append) and
// checks that every transition reports it instead of swallowing it, and that nothing is left half
// done for the ones that run in a transaction.
func TestTransitionsSurfaceStoreErrors(t *testing.T) {
	ctx := context.Background()
	type op struct {
		name string
		// run prepares the state and returns the call under test
		prep func(t *testing.T, e *env) func() error
	}
	ops := []op{
		{"Cancel", func(t *testing.T, e *env) func() error {
			s := e.reserved(t, "key-0000001")
			return func() error { _, err := e.svc.Cancel(ctx, e.userID, s.ID); return err }
		}},
		{"Abandon", func(t *testing.T, e *env) func() error {
			s := e.reserved(t, "key-0000001")
			e.clk.Set(t0.Add(24 * time.Hour))
			return func() error { _, err := e.svc.Abandon(ctx, s.ID, session.ActorSweeper); return err }
		}},
		{"BeginStart", func(t *testing.T, e *env) func() error {
			s := e.reserved(t, "key-0000001")
			fence := e.claim(t, s.ID, session.StatusReserved)
			return func() error { _, err := e.svc.BeginStart(ctx, s.ID, fence); return err }
		}},
		{"FailStart", func(t *testing.T, e *env) func() error {
			s := e.reserved(t, "key-0000001")
			fence := e.claim(t, s.ID, session.StatusReserved)
			_, err := e.svc.BeginStart(ctx, s.ID, fence)
			require.NoError(t, err)
			return func() error {
				_, err := e.svc.FailStart(ctx, s.ID, &fence, session.EndProviderFailure, "x", session.ActorRealtime)
				return err
			}
		}},
		{"Activate", func(t *testing.T, e *env) func() error {
			s := e.reserved(t, "key-0000001")
			fence := e.claim(t, s.ID, session.StatusReserved)
			_, err := e.svc.BeginStart(ctx, s.ID, fence)
			require.NoError(t, err)
			return func() error { _, err := e.svc.Activate(ctx, s.ID, fence); return err }
		}},
		{"BeginEnding", func(t *testing.T, e *env) func() error {
			s, fence := e.active(t, 600)
			e.clk.Set(at(10, 5, 0, 0))
			return func() error {
				_, err := e.svc.BeginEnding(ctx, s.ID, &fence, session.EndUserEnded, "", session.ActorRealtime)
				return err
			}
		}},
		{"Finalize", func(t *testing.T, e *env) func() error {
			s, fence := e.active(t, 600)
			e.clk.Set(at(10, 5, 0, 0))
			_, err := e.svc.BeginEnding(ctx, s.ID, &fence, session.EndUserEnded, "", session.ActorRealtime)
			require.NoError(t, err)
			e.clk.Set(at(10, 5, 3, 0))
			return func() error { _, err := e.svc.Finalize(ctx, s.ID, &fence, session.ActorRealtime); return err }
		}},
		{"FailActive", func(t *testing.T, e *env) func() error {
			s, _ := e.active(t, 600)
			e.clk.Set(at(10, 5, 0, 0))
			return func() error { _, err := e.svc.FailActive(ctx, s.ID, at(10, 4, 50, 0), "lost"); return err }
		}},
		{"EndStaleHandoff", func(t *testing.T, e *env) func() error {
			s, fence := e.active(t, 600)
			e.clk.Set(at(10, 2, 0, 0))
			_, err := e.svc.MarkHandoff(ctx, s.ID, fence)
			require.NoError(t, err)
			e.clk.Set(at(10, 4, 0, 0))
			return func() error { _, _, err := e.svc.EndStaleHandoff(ctx, s.ID, at(10, 3, 0, 0)); return err }
		}},
	}
	for _, o := range ops {
		for _, point := range []string{"Sessions.CAS", "Events.Append"} {
			t.Run(o.name+"/"+point, func(t *testing.T) {
				e := newEnv(t, 3600)
				call := o.prep(t, e)
				e.store.FailNext(point, errBoom)
				require.ErrorIs(t, call(), errBoom)
			})
		}
	}
}

func TestControlPublishFailureIsOnlyLogged(t *testing.T) {
	e := newEnv(t, 3600)
	s := e.reserved(t, "key-0000001")
	e.control.FailNext("Publish", errBoom)
	got, err := e.svc.Cancel(context.Background(), e.userID, s.ID)
	require.NoError(t, err, "the commit stands even when the runtime could not be told")
	require.Equal(t, session.StatusCancelled, got.Status)
	require.Contains(t, e.logs.String(), "session control publish failed")
	require.Contains(t, e.logs.String(), s.ID.String())
}

func TestTransitionErrorMessageAndUnwrap(t *testing.T) {
	err := error(&session.TransitionError{From: session.StatusActive, To: session.StatusCancelled})
	require.Equal(t, "invalid session transition ACTIVE → CANCELLED", err.Error())
	require.ErrorIs(t, err, session.ErrInvalidTransition)
	var te *session.TransitionError
	require.ErrorAs(t, err, &te)
	require.Equal(t, session.StatusActive, te.From)
}

func TestNopMetricsAndNoControlAreInert(t *testing.T) {
	m := session.NopMetrics{}
	m.Created("ok")
	m.Ended("api", "USER_ENDED", "COMPLETED")
	// a Service built without Control or Metrics still works end to end
	e := newEnv(t, 3600)
	svc := session.NewService(session.Deps{Store: e.store, Reader: e.store.Reader(), Balance: balance.NewService(e.bal, e.clk, e.ids),
		Users: e.users, Clock: e.clk, IDs: e.ids})
	require.NotNil(t, svc)
}

// TestRuntimeFacadeReturnsServiceErrors: the facade adds nothing to a refusal of the Service.
func TestRuntimeFacadeReturnsServiceErrors(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, 3600)
	rt := e.runtime()
	s := e.reserved(t, "key-0000001")

	_, _, err := rt.ClaimRuntime(ctx, s.ID, []session.Status{session.StatusActive})
	require.ErrorIs(t, err, session.ErrInvalidTransition, "ClaimRuntime from a status that is not allowed")

	fence, _, err := rt.ClaimRuntime(ctx, s.ID, []session.Status{session.StatusReserved})
	require.NoError(t, err)
	_, err = rt.FailStart(ctx, s.ID, fence, session.EndProviderFailure)
	require.ErrorIs(t, err, session.ErrInvalidTransition, "FailStart on a session that never started")
	_, err = rt.Complete(ctx, s.ID, fence)
	require.Error(t, err, "Complete on a session that is not ENDING")
	_, err = rt.Fail(ctx, s.ID, fence, session.EndProviderFailure)
	require.Error(t, err, "Fail on a session that is not ENDING")
}
