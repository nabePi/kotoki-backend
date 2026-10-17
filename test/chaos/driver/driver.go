// Package driver performs the Actions of a chaos scenario: the fake provider fault document in
// Redis, commands against docker compose or a Kubernetes cluster, and the probes of the harness.
// The command drivers only build argv and hand it to an injected Run; they refuse every command
// outside the small set a scenario needs, so that a typo in the table cannot delete something that
// is not meant to go (the machine of the operator runs other compose projects).
package driver

import (
	"context"
	"errors"
	"fmt"

	"github.com/nabePi/kotoki-backend/test/chaos/scenario"
)

// Multi routes an Action to the driver of its Kind. A kind without a driver is an error.
type Multi struct {
	Fault, Cmd, Probe scenario.Driver
}

// Do implements scenario.Driver.
func (m Multi) Do(ctx context.Context, a scenario.Action) error {
	var d scenario.Driver
	switch a.Kind {
	case scenario.KindFault, scenario.KindReset:
		d = m.Fault
	case scenario.KindCmd:
		d = m.Cmd
	case scenario.KindProbe:
		d = m.Probe
	default:
		return fmt.Errorf("driver: unknown action kind %q", a.Kind)
	}
	if d == nil {
		return fmt.Errorf("driver: no driver for %q actions", a.Kind)
	}
	return d.Do(ctx, a)
}

var errNotCmd = errors.New("driver: not a cmd action")
