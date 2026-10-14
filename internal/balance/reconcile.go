package balance

import (
	"context"
	"fmt"
)

// ReconcileRepository runs one invariant query of spec 4.8 and returns the offending rows as
// short descriptions (no personal data). check is "R1".."R6"; the SQL lives in PostgreSQL.
type ReconcileRepository interface {
	RunCheck(ctx context.Context, check string) ([]string, error)
}

// Violation lists the rows that break one invariant.
type Violation struct {
	Check string
	Rows  []string
}

var reconcileChecks = []string{"R1", "R2", "R3", "R4", "R5", "R6"}

// Reconcile runs R1–R6 in order and returns the violated checks (nil when the ledger is
// consistent). The first failing query stops the run and its error names the check. The gauge
// kotoki_balance_invariant_violations is set by the M6 job, not here.
func (s *Service) Reconcile(ctx context.Context, r ReconcileRepository) ([]Violation, error) {
	var out []Violation
	for _, c := range reconcileChecks {
		rows, err := r.RunCheck(ctx, c)
		if err != nil {
			return nil, fmt.Errorf("balance: reconcile %s: %w", c, err)
		}
		if len(rows) > 0 {
			out = append(out, Violation{Check: c, Rows: rows})
		}
	}
	return out, nil
}
