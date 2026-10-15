package session

// transitions is the table of spec 5.5: the only source of truth for what may follow what.
var transitions = map[Status]map[Status]bool{
	StatusReserved: {StatusStarting: true, StatusCancelled: true, StatusAbandoned: true},
	StatusStarting: {StatusActive: true, StatusFailed: true},
	StatusActive:   {StatusEnding: true, StatusFailed: true},
	StatusEnding:   {StatusCompleted: true, StatusFailed: true},
}

// CanTransition reports whether from → to is allowed. Service and the tests use it; nothing else
// decides.
func CanTransition(from, to Status) bool { return transitions[from][to] }

// TerminalStatusFor maps an end reason to the terminal status it leads to (spec 5.4).
func TerminalStatusFor(r EndReason) Status {
	switch r {
	case EndUserEnded, EndDurationElapsed, EndReconnectTimeout:
		return StatusCompleted
	case EndProviderFailure, EndServerError:
		return StatusFailed
	case EndUserCancelled:
		return StatusCancelled
	case EndNotStarted:
		return StatusAbandoned
	}
	return ""
}
