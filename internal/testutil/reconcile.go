//go:build integration || e2e

package testutil

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// reconcileChecks are checks R1–R5 of spec 4.8, verbatim. Every query returns
// violating rows, so each must come back empty.
var reconcileChecks = []struct{ name, sql string }{
	{"R1", `SELECT a.user_id, a.available_seconds, a.reserved_seconds,
       COALESCE(l.sum_available, 0) AS ledger_available,
       COALESCE(l.sum_reserved, 0)  AS ledger_reserved
  FROM balance_accounts a
  LEFT JOIN (SELECT account_id,
                    SUM(available_seconds_delta) AS sum_available,
                    SUM(reserved_seconds_delta)  AS sum_reserved
               FROM balance_ledger GROUP BY account_id) l ON l.account_id = a.id
 WHERE a.available_seconds <> COALESCE(l.sum_available, 0)
    OR a.reserved_seconds  <> COALESCE(l.sum_reserved, 0)`},
	{"R2", `SELECT a.user_id, a.reserved_seconds, COALESCE(h.held, 0) AS held
  FROM balance_accounts a
  LEFT JOIN (SELECT account_id, SUM(reserved_seconds) AS held
               FROM balance_reservations WHERE status = 'HELD' GROUP BY account_id) h
         ON h.account_id = a.id
 WHERE a.reserved_seconds <> COALESCE(h.held, 0)`},
	{"R3", `SELECT a.user_id
  FROM balance_accounts a
  LEFT JOIN LATERAL (SELECT available_seconds_after, reserved_seconds_after, account_version
                       FROM balance_ledger WHERE account_id = a.id
                      ORDER BY account_version DESC LIMIT 1) last ON true
 WHERE COALESCE(last.account_version, 0)          <> a.version
    OR COALESCE(last.available_seconds_after, 0)  <> a.available_seconds
    OR COALESCE(last.reserved_seconds_after, 0)   <> a.reserved_seconds`},
	{"R4", `SELECT r.id, r.status AS reservation_status, s.status AS session_status, s.started_at
  FROM balance_reservations r JOIN sessions s ON s.id = r.session_id
 WHERE NOT (
       (r.status = 'HELD'     AND s.status IN ('RESERVED','STARTING','ACTIVE','ENDING'))
    OR (r.status = 'RELEASED' AND (s.status IN ('CANCELLED','ABANDONED')
                                   OR (s.status = 'FAILED' AND s.started_at IS NULL)))
    OR (r.status = 'SETTLED'  AND s.status IN ('COMPLETED','FAILED') AND s.started_at IS NOT NULL))`},
	{"R5", `SELECT r.id
  FROM balance_reservations r
  JOIN sessions s ON s.id = r.session_id
  LEFT JOIN (SELECT reservation_id,
                    SUM(reserved_seconds_delta) AS sum_reserved,
                    -SUM(reserved_seconds_delta) FILTER (WHERE entry_type = 'CONSUME') AS consumed
               FROM balance_ledger WHERE reservation_id IS NOT NULL GROUP BY reservation_id) l
         ON l.reservation_id = r.id
 WHERE (r.status <> 'HELD' AND (s.charged_seconds <> r.consumed_seconds
                                OR COALESCE(l.sum_reserved, 0) <> 0
                                OR COALESCE(l.consumed, 0) <> r.consumed_seconds))
    OR (r.status = 'HELD' AND COALESCE(l.sum_reserved, 0) <> r.reserved_seconds)`},
}

// AssertReconciled fails the test if any balance invariant check R1–R5 reports a row.
func AssertReconciled(t testing.TB, pool *pgxpool.Pool) {
	t.Helper()
	for _, c := range reconcileChecks {
		rows, err := pool.Query(context.Background(), c.sql)
		if err != nil {
			t.Fatalf("testutil: reconcile %s: %v", c.name, err)
		}
		n := 0
		for rows.Next() {
			n++
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatalf("testutil: reconcile %s: %v", c.name, err)
		}
		if n > 0 {
			t.Fatalf("testutil: balance invariant %s violated by %d row(s)", c.name, n)
		}
	}
}
