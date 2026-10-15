package conversation

// The rolling summary prompt template itself lives in BuildRollingSummary (context_builder.go,
// spec 7.8.5); this file holds the trigger arithmetic.

const defaultSummaryTriggerTurns = 4 // ROLLING_SUMMARY_TRIGGER_TURNS

// uncoveredBefore counts the non-DISCARDED turns with covered < seq <= oldestSeqInWindow-1: the
// turns that fell out of the recent window and no summary covers yet.
func uncoveredBefore(turns []TurnContext, covered, oldestSeqInWindow int) int {
	n := 0
	for _, t := range turns {
		if t.Status != statusDiscarded && t.Seq <= oldestSeqInWindow-1 && t.Seq > covered {
			n++
		}
	}
	return n
}

// coveredUntil is the highest seq among the non-DISCARDED turns (0 when there are none); it is
// the coveredUntilSeq SelectRecentWindow consumes once a summary of these turns is stored.
func coveredUntil(turns []TurnContext) int {
	hi := 0
	for _, t := range turns {
		if t.Status != statusDiscarded {
			hi = max(hi, t.Seq)
		}
	}
	return hi
}
