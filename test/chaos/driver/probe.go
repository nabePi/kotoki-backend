package driver

import (
	"context"
	"fmt"

	"github.com/nabePi/kotoki-backend/test/chaos/scenario"
)

// Probe performs KindProbe actions: Argv[0] names the probe in Funcs, the rest are its arguments.
// A probe fails the action (and so the run) when what it observes is wrong.
type Probe struct {
	Funcs map[string]func(ctx context.Context, args []string) error
}

// Do implements scenario.Driver.
func (p Probe) Do(ctx context.Context, a scenario.Action) error {
	if a.Kind != scenario.KindProbe || len(a.Argv) == 0 {
		return fmt.Errorf("driver: a probe action needs a name in Argv[0]")
	}
	f, ok := p.Funcs[a.Argv[0]]
	if !ok {
		return fmt.Errorf("driver: unknown probe %q", a.Argv[0])
	}
	return f(ctx, a.Argv[1:])
}
