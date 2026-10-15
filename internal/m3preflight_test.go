package internal_test

import (
	"context"
	"testing"

	"github.com/redis/go-redis/v9"

	"github.com/nabePi/kotoki-backend/internal/admin"
	"github.com/nabePi/kotoki-backend/internal/auth"
	"github.com/nabePi/kotoki-backend/internal/balance"
	"github.com/nabePi/kotoki-backend/internal/httpapi"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
	"github.com/nabePi/kotoki-backend/internal/prompt"
	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/testutil/fakes"
)

// Compiling this file proves the M1/M2/M4 contracts M3 builds on still exist with the shapes it
// expects; a compile error here means a milestone drifted (fix there, record a deviation).
var (
	_ errs.Code                                           = errs.CodeBalanceInsufficient
	_                                                     = errs.Messages
	_ clock.Clock                                         = clock.System{}
	_ idgen.Generator                                     = idgen.NewV7()
	_ redis.UniversalClient                               = (redis.UniversalClient)(nil)
	_ func(errs.Code) int                                 = httpapi.StatusOf
	_ auth.Principal                                      = auth.Principal{}
	_ balance.Store                                       = balance.Store(nil)
	_ balance.Tx                                          = balance.Tx(nil)
	_ balance.Repository                                  = balance.Repository(nil)
	_ *balance.Service                                    = (*balance.Service)(nil)
	_ admin.SessionGate                                   = admin.SessionGate(nil)
	_ func(testing.TB, any)                               = nil
	_                                                     = fakes.NewBalanceStore
	_ func(prompt.Template, any) (prompt.Rendered, error) = prompt.Render
	_ provider.VoiceSelection                             = provider.VoiceSelection{}
	_                                                     = context.Background
)

func TestPreflightCompiles(*testing.T) {}
