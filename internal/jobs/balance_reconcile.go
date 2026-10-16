package jobs

import (
	"context"
	"log/slog"

	"github.com/nabePi/kotoki-backend/internal/balance"
)

const maxLoggedViolationRows = 20

// balanceChecks are the invariant checks R1..R6 of spec 4.8; each has a gauge series.
var balanceChecks = []string{"R1", "R2", "R3", "R4", "R5", "R6"}

// Reconciler runs the balance invariant checks (balance.Service.Reconcile bound to its repository).
type Reconciler interface {
	Reconcile(ctx context.Context) ([]balance.Violation, error)
}

type boundReconciler struct {
	svc  *balance.Service
	repo balance.ReconcileRepository
}

func (b boundReconciler) Reconcile(ctx context.Context) ([]balance.Violation, error) {
	return b.svc.Reconcile(ctx, b.repo)
}

// BindReconciler binds the balance service to the repository that holds the R1..R6 queries.
func BindReconciler(svc *balance.Service, repo balance.ReconcileRepository) Reconciler {
	return boundReconciler{svc: svc, repo: repo}
}

// BalanceReconcileJob reports ledger invariant violations as gauges and ERROR logs. It only reads:
// nothing is ever corrected automatically (spec 4.8).
type BalanceReconcileJob struct {
	r   Reconciler
	m   *Metrics
	log *slog.Logger
}

// NewBalanceReconcileJob builds the job.
func NewBalanceReconcileJob(r Reconciler, m *Metrics, log *slog.Logger) *BalanceReconcileJob {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &BalanceReconcileJob{r: r, m: m, log: log}
}

// Run implements Job. After a clean run every gauge is 0. When the reconciler returns an error
// (some check failed to run) the violations it did find are still published and the error is
// returned; the gauges of checks without a reported violation are then left as they were, since
// a check that did not run proves nothing and 0 would hide an earlier finding.
func (j *BalanceReconcileJob) Run(ctx context.Context) error {
	violations, err := j.r.Reconcile(ctx)
	byCheck := make(map[string]balance.Violation, len(violations))
	for _, v := range violations {
		byCheck[v.Check] = v
	}
	for _, c := range balanceChecks {
		v, found := byCheck[c]
		switch {
		case found:
			j.m.BalanceInvariantViolations.WithLabelValues(c).Set(float64(len(v.Rows)))
		case err == nil:
			j.m.BalanceInvariantViolations.WithLabelValues(c).Set(0)
		}
	}
	for _, v := range violations {
		rows := v.Rows
		if len(rows) > maxLoggedViolationRows {
			rows = rows[:maxLoggedViolationRows]
		}
		j.log.ErrorContext(ctx, "balance.reconcile.mismatch", "check", v.Check, "rows", len(v.Rows), "ids", rows)
	}
	return err
}
