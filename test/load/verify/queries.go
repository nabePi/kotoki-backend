//go:build !windows

package verify

// Every check returns one text row per offending object (ids and numbers only, no user data). Checks
// V3-V8 take $1 = Since (the zero time when no cutoff) and look at sessions created at or after it;
// V1 and V2 look at the accounts as they are. The SQL is written out here on purpose: the tool is
// independent of the balance package (spec 13.12.6, restore drill 13.3.4).

// check is one read-only check.
type check struct {
	ID    string
	SQL   string
	Since bool // takes $1
}

const rowLimit = " LIMIT 100"

var checks = []check{
	{ID: "V1", SQL: `
SELECT 'account_id=' || a.id::text || ' available=' || a.available_seconds::text || '/ledger ' || COALESCE(l.av, 0)::text ||
       ' reserved=' || a.reserved_seconds::text || '/ledger ' || COALESCE(l.rs, 0)::text
  FROM balance_accounts a
  LEFT JOIN (SELECT account_id, SUM(available_seconds_delta) AS av, SUM(reserved_seconds_delta) AS rs
               FROM balance_ledger GROUP BY account_id) l ON l.account_id = a.id
 WHERE a.available_seconds <> COALESCE(l.av, 0) OR a.reserved_seconds <> COALESCE(l.rs, 0)` + rowLimit},

	{ID: "V2", SQL: `
SELECT 'account_id=' || id::text || ' available=' || available_seconds::text || ' reserved=' || reserved_seconds::text
  FROM balance_accounts WHERE available_seconds < 0 OR reserved_seconds < 0` + rowLimit},

	{ID: "V3", Since: true, SQL: `
SELECT 'reservation_id=' || r.id::text || ' session_id=' || s.id::text || ' session_status=' || s.status
  FROM balance_reservations r JOIN sessions s ON s.id = r.session_id
 WHERE r.status = 'HELD' AND s.status IN ('COMPLETED', 'CANCELLED', 'ABANDONED', 'FAILED') AND s.created_at >= $1` + rowLimit},

	{ID: "V4", Since: true, SQL: `
SELECT 'reservation_id=' || l.reservation_id::text || ' consume=' || COUNT(*) FILTER (WHERE l.entry_type = 'CONSUME')::text ||
       ' release=' || COUNT(*) FILTER (WHERE l.entry_type = 'RELEASE')::text
  FROM balance_ledger l JOIN sessions s ON s.id = l.session_id
 WHERE s.status IN ('COMPLETED', 'CANCELLED', 'ABANDONED', 'FAILED') AND s.created_at >= $1 AND l.reservation_id IS NOT NULL
 GROUP BY l.reservation_id
HAVING COUNT(*) FILTER (WHERE l.entry_type = 'CONSUME') > 1 OR COUNT(*) FILTER (WHERE l.entry_type = 'RELEASE') > 1` + rowLimit},

	{ID: "V5", Since: true, SQL: `
SELECT 'session_id=' || id::text || ' charged=' || charged_seconds::text || ' reserved=' || reserved_seconds::text
  FROM sessions WHERE status = 'COMPLETED' AND charged_seconds > reserved_seconds AND created_at >= $1` + rowLimit},

	{ID: "V6", Since: true, SQL: `
SELECT 'session_id=' || t.session_id::text || ' sequence_number=' || t.sequence_number::text || ' rows=' || COUNT(*)::text
  FROM session_turns t JOIN sessions s ON s.id = t.session_id
 WHERE s.created_at >= $1
 GROUP BY t.session_id, t.sequence_number HAVING COUNT(*) > 1` + rowLimit},

	// V7: a session with two evaluations, or a COMPLETED evaluation (its source_event_id is the
	// session.completed event it came from) with no processed_events row of the evaluation consumer.
	{ID: "V7", Since: true, SQL: `
SELECT 'session_id=' || e.session_id::text || ' evaluations=' || COUNT(*)::text
  FROM evaluations e JOIN sessions s ON s.id = e.session_id
 WHERE s.created_at >= $1
 GROUP BY e.session_id HAVING COUNT(*) > 1
UNION ALL
SELECT 'session_id=' || e.session_id::text || ' evaluation_id=' || e.id::text || ' without processed_events'
  FROM evaluations e JOIN sessions s ON s.id = e.session_id
 WHERE s.created_at >= $1 AND e.status = 'COMPLETED'
   AND NOT EXISTS (SELECT 1 FROM processed_events p
                    WHERE p.consumer = 'evaluation.session_completed' AND p.event_id = e.source_event_id)` + rowLimit},

	// V8: every COMPLETED session must end up with a COMPLETED evaluation (a lost session.completed event
	// shows up here too). It is only run when Options.AwaitEvaluations > 0 (K13).
	{ID: "V8", Since: true, SQL: `
SELECT 'session_id=' || s.id::text || ' has no COMPLETED evaluation'
  FROM sessions s
 WHERE s.status = 'COMPLETED' AND s.created_at >= $1
   AND NOT EXISTS (SELECT 1 FROM evaluations e WHERE e.session_id = s.id AND e.status = 'COMPLETED')` + rowLimit},
}
