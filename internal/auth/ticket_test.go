package auth_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/auth"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/testutil/fakes"
)

type ticketEnv struct {
	svc     *auth.TicketService
	ae      *authEnv
	reader  *fakes.TicketSessionReader
	tickets *fakes.TicketStore
}

func newTicketEnv(t *testing.T) *ticketEnv {
	t.Helper()
	ae := newAuthEnv(t)
	reader, tickets := fakes.NewTicketSessionReader(), fakes.NewTicketStore()
	svc := auth.NewTicketService(ae.store, reader, tickets, ae.clk,
		auth.TicketConfig{TTL: 30 * time.Second, RealtimeURL: "wss://rt.example"})
	return &ticketEnv{svc: svc, ae: ae, reader: reader, tickets: tickets}
}

func TestTicketIssue(t *testing.T) {
	e := newTicketEnv(t)
	reg := e.ae.register(t, "hana@example.com")
	uid, sid := reg.User.ID, newUUID()
	ctx := context.Background()
	deadline := t0.Add(5 * time.Minute)

	// unknown session
	_, err := e.svc.Issue(ctx, uid, sid)
	require.Equal(t, errs.CodeSessionNotFound, errs.CodeOf(err))

	// RESERVED before the deadline
	e.reader.Set(sid, uid, auth.TicketTarget{Status: "RESERVED", StartDeadlineAt: deadline})
	res, err := e.svc.Issue(ctx, uid, sid)
	require.NoError(t, err)
	require.Len(t, res.Ticket, 43)
	require.Equal(t, 30, res.ExpiresIn)
	require.Equal(t, "wss://rt.example/api/v1/realtime", res.RealtimeURL)
	require.Equal(t, []auth.WSTicket{{UserID: uid, SessionID: sid, IssuedAt: t0}}, e.tickets.Issued)
	require.Equal(t, []time.Duration{30 * time.Second}, e.tickets.TTLs)

	// RESERVED exactly at the deadline
	e.ae.clk.Set(deadline)
	_, err = e.svc.Issue(ctx, uid, sid)
	require.Equal(t, errs.CodeSessionStartDeadlinePassed, errs.CodeOf(err))
	e.ae.clk.Set(t0)

	for _, st := range []string{"STARTING", "ACTIVE"} {
		e.reader.Set(sid, uid, auth.TicketTarget{Status: st})
		_, err = e.svc.Issue(ctx, uid, sid)
		require.NoError(t, err, st)
	}
	for _, st := range []string{"ENDING", "COMPLETED", "CANCELLED", "ABANDONED", "FAILED"} {
		e.reader.Set(sid, uid, auth.TicketTarget{Status: st})
		_, err = e.svc.Issue(ctx, uid, sid)
		require.Equal(t, errs.CodeSessionNotConnectable, errs.CodeOf(err), st)
		require.Equal(t, map[string]any{"status": st}, errs.DetailsOf(err))
	}
}

func TestTicketIssueStaleTermsStillOK(t *testing.T) {
	e := newTicketEnv(t)
	reg := e.ae.register(t, "hana@example.com")
	e.ae.store.MutateUser(reg.User.ID, func(u *auth.User) { u.TermsVersion = "2020-01-01" })
	sid := newUUID()
	e.reader.Set(sid, reg.User.ID, auth.TicketTarget{Status: "ACTIVE"})
	_, err := e.svc.Issue(context.Background(), reg.User.ID, sid)
	require.NoError(t, err)
}

func TestTicketIssueInactiveUser(t *testing.T) {
	e := newTicketEnv(t)
	reg := e.ae.register(t, "hana@example.com")
	sid := newUUID()
	e.reader.Set(sid, reg.User.ID, auth.TicketTarget{Status: "ACTIVE"})
	for _, st := range []auth.UserStatus{auth.UserStatusSuspended, auth.UserStatusDeleted} {
		e.ae.store.MutateUser(reg.User.ID, func(u *auth.User) { u.Status = st })
		_, err := e.svc.Issue(context.Background(), reg.User.ID, sid)
		require.Equal(t, errs.CodeAccountSuspended, errs.CodeOf(err), string(st))
	}
}

func TestTicketIssueStoreErrorIsServiceUnavailable(t *testing.T) {
	e := newTicketEnv(t)
	reg := e.ae.register(t, "hana@example.com")
	sid := newUUID()
	e.reader.Set(sid, reg.User.ID, auth.TicketTarget{Status: "ACTIVE"})
	e.tickets.FailNext("Issue", errors.New("redis down"))
	_, err := e.svc.Issue(context.Background(), reg.User.ID, sid)
	require.Equal(t, errs.CodeServiceUnavailable, errs.CodeOf(err))
}
