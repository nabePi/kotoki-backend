package balance

import "time"

// BillableEnd = LEAST(COALESCE(endingAt, endedAt), expiresAt) (D-036, spec 4.5): the only place
// of this formula. endingAt is nil only for ACTIVE→FAILED by the sweeper, where endedAt =
// LEAST(last_heartbeat_at, expires_at).
func BillableEnd(endingAt *time.Time, endedAt, expiresAt time.Time) time.Time {
	end := endedAt
	if endingAt != nil {
		end = *endingAt
	}
	if expiresAt.Before(end) {
		end = expiresAt
	}
	return end
}

// ComputeCharge returns the seconds to charge for a session that reserved `reserved` seconds
// (spec 4.5). serviceFault is true when end_reason ∈ {PROVIDER_FAILURE, SERVER_ERROR}; the
// caller (session.Service) maps it, because balance does not import session. Sessions that
// never became ACTIVE are not charged by the caller and never reach this function.
func ComputeCharge(reserved int64, startedAt, billableEnd time.Time, disconnectedSeconds int64, serviceFault bool) int64 {
	activeMs := billableEnd.Sub(startedAt).Milliseconds() - disconnectedSeconds*1000
	if activeMs < 0 {
		activeMs = 0
	}
	charged := (activeMs + 999) / 1000
	if serviceFault {
		if activeMs < 60_000 {
			return 0
		}
		charged -= 30 // activeMs ≥ 60 s here, so charged ≥ 60 and never goes negative
	}
	if charged > reserved {
		charged = reserved
	}
	return charged
}
