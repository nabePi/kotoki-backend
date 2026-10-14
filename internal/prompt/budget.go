package prompt

// Budget holds the per-component token budget of the prompt (spec 7.8.1).
type Budget struct {
	System, Character, Scenario, Learner,
	MemoryCore, MemoryEpisodic, Summary, SessionState, RecentTurns, CurrentInput int
	MinRecentTurns int // recent turns kept when trimming (never fewer than this)
}

// DefaultBudget is the table of spec 7.8.1: total input 6,450 tokens. Bab 11 reads MemoryCore
// and MemoryEpisodic from here.
var DefaultBudget = Budget{
	System: 600, Character: 500, Scenario: 400, Learner: 700,
	MemoryCore: 600, MemoryEpisodic: 500, Summary: 500, SessionState: 50, RecentTurns: 2000, CurrentInput: 600,
	MinRecentTurns: 4,
}

// Total is the sum of the ten components (MinRecentTurns is a count, not tokens).
func (b Budget) Total() int {
	return b.System + b.Character + b.Scenario + b.Learner + b.MemoryCore + b.MemoryEpisodic +
		b.Summary + b.SessionState + b.RecentTurns + b.CurrentInput
}
