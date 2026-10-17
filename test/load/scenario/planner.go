//go:build !windows

// Package scenario plans what one virtual user does in one session (spec 13.12.2): the session
// length and start mode, the utterances with their pauses, barge-ins and fillers, an optional
// connection drop, and how the session ends. A Planner is deterministic for a given seed.
package scenario

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/nabePi/kotoki-backend/internal/kotokiclient"
)

// Model constants of spec 13.12.2.
const (
	// BargeInFixture is the utterance spoken on top of the AI in a barge-in turn.
	BargeInFixture = "ja_barge_in"
	// ExpiryMargin is how long a session that waits for DURATION_ELAPSED is given beyond its length.
	ExpiryMargin = 30 * time.Second

	// estimatedTurn is the planner's guess of what the AI adds to a turn (answer and playback), used
	// only to decide how many turns fill a session.
	estimatedTurn = 4 * time.Second

	minSpeech = 2 * time.Second
	maxSpeech = 8 * time.Second
	minEnd    = 30 * time.Second
)

const (
	pBargeIn    = 0.10
	pFiller     = 0.15
	pDrop       = 0.02
	pWaitExpiry = 0.70
	pAIFirst    = 0.50
)

// Fixture is one entry of testdata/audio/<dir>/manifest.json.
type Fixture struct {
	Name       string
	DurationMS int
	Tags       []string
}

// LoadFixtures reads manifest.json of dir, or of dir/synth when dir has none (the fixtures live in
// testdata/audio/synth). It returns the directory the manifest was found in.
func LoadFixtures(dir string) ([]Fixture, string, error) {
	for _, d := range []string{dir, filepath.Join(dir, "synth")} {
		raw, err := os.ReadFile(filepath.Join(d, "manifest.json")) //nolint:gosec // G304: the operator chooses the fixture directory
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, "", err
		}
		var m struct {
			Fixtures []struct {
				Name       string   `json:"name"`
				DurationMS int      `json:"duration_ms"`
				Tags       []string `json:"tags"`
			} `json:"fixtures"`
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, "", fmt.Errorf("scenario: %s/manifest.json: %w", d, err)
		}
		out := make([]Fixture, 0, len(m.Fixtures))
		for _, f := range m.Fixtures {
			out = append(out, Fixture{Name: f.Name, DurationMS: f.DurationMS, Tags: f.Tags})
		}
		return out, d, nil
	}
	return nil, "", fmt.Errorf("scenario: no manifest.json in %s or %s", dir, filepath.Join(dir, "synth"))
}

// SessionPlan is the script of one session.
type SessionPlan struct {
	DurationSeconds int
	StartMode       string // USER_FIRST | AI_FIRST
	// EndAfter is when the user ends the session (session.end), measured from the start; 0 waits for
	// DURATION_ELAPSED. It also sets how many turns the plan holds.
	EndAfter time.Duration
	Turns    []TurnPlan
	Drop     *DropPlan
}

// TurnPlan is one utterance. Think is the pause after the AI finished, before this utterance. A turn
// with BargeInAfter > 0 interrupts the AI's answer to the previous utterance that long after its
// first audio frame; its Fixture is BargeInFixture.
type TurnPlan struct {
	Fixture      string
	Think        time.Duration
	BargeInAfter time.Duration
	Filler       bool
}

// DropPlan breaks the connection once, right after the utterance of turn AfterTurn, and reconnects
// after For.
type DropPlan struct {
	AfterTurn int
	For       time.Duration
}

// Planner draws SessionPlans. It is safe for concurrent use, but one Planner per VU (ForVU) keeps
// every VU's sequence independent of scheduling.
type Planner struct {
	seed     int64
	fixtures []Fixture
	speech   []Fixture
	filler   []Fixture
	hasBarge bool

	mu  sync.Mutex
	rng *rand.Rand
}

// NewPlanner returns a Planner seeded with seed that draws from fixtures.
func NewPlanner(seed int64, fixtures []Fixture) *Planner {
	p := &Planner{seed: seed, fixtures: slices.Clone(fixtures)}
	//nolint:gosec // G404: a load model, not a security decision
	p.rng = rand.New(rand.NewPCG(uint64(seed), 0x6b6f746f6b69))
	excluded := []string{"noise", "silence", "filler", "barge-in", "long", "max-utterance"}
	hasTag := func(f Fixture, tag string) bool { return slices.Contains(f.Tags, tag) }
	var relaxed []Fixture
	for _, f := range fixtures {
		switch {
		case f.Name == BargeInFixture:
			p.hasBarge = true
		case hasTag(f, "filler") && hasTag(f, "speech"):
			p.filler = append(p.filler, f)
		case hasTag(f, "speech") && !slices.ContainsFunc(excluded, func(t string) bool { return hasTag(f, t) }):
			relaxed = append(relaxed, f)
			if d := time.Duration(f.DurationMS) * time.Millisecond; d >= minSpeech && d <= maxSpeech {
				p.speech = append(p.speech, f)
			}
		}
	}
	if len(p.speech) == 0 {
		p.speech = relaxed // none is 2-8 s: any short enough speech fixture beats none
	}
	return p
}

// ForVU returns a Planner with the same fixtures and seed+i, the sequence of VU number i.
func (p *Planner) ForVU(i int) *Planner { return NewPlanner(p.seed+int64(i), p.fixtures) }

// FixtureNames lists, sorted, the fixtures a plan can use: the generator pre-encodes exactly these.
func (p *Planner) FixtureNames() []string {
	var names []string
	for _, f := range p.speech {
		names = append(names, f.Name)
	}
	for _, f := range p.filler {
		names = append(names, f.Name)
	}
	if p.hasBarge && len(p.speech) > 0 {
		names = append(names, BargeInFixture)
	}
	slices.Sort(names)
	return names
}

func (p *Planner) between(lo, hi time.Duration) time.Duration {
	return lo + time.Duration(p.rng.Int64N(int64(hi-lo)+1))
}

// Next draws the plan of the next session. A Planner without a speech fixture returns a plan
// without turns.
func (p *Planner) Next() SessionPlan {
	p.mu.Lock()
	defer p.mu.Unlock()

	s := SessionPlan{StartMode: "USER_FIRST"}
	switch u := p.rng.Float64(); {
	case u < 0.6:
		s.DurationSeconds = 300
	case u < 0.9:
		s.DurationSeconds = 600
	default:
		s.DurationSeconds = 900
	}
	if p.rng.Float64() < pAIFirst {
		s.StartMode = "AI_FIRST"
	}
	length := time.Duration(s.DurationSeconds) * time.Second
	if p.rng.Float64() >= pWaitExpiry {
		s.EndAfter = p.between(minEnd, length*8/10)
	}
	target := length
	if s.EndAfter > 0 {
		target = s.EndAfter
	}

	if len(p.speech) > 0 {
		for est := time.Duration(0); est < target; {
			t := p.turn(len(s.Turns))
			est += t.Think + estimatedTurn + p.length(t.Fixture)
			s.Turns = append(s.Turns, t)
		}
		if p.rng.Float64() < pDrop {
			s.Drop = &DropPlan{AfterTurn: p.rng.IntN(len(s.Turns)), For: p.between(time.Second, 5*time.Second)}
		}
	}
	return s
}

func (p *Planner) length(name string) time.Duration {
	for _, f := range p.fixtures {
		if f.Name == name {
			return time.Duration(f.DurationMS) * time.Millisecond
		}
	}
	return 0
}

// turn draws turn number i. Barge-in and filler are exclusive: a barge-in turn (10% of the turns
// after the first) speaks ja_barge_in, and the filler share among the others is raised so that
// filler is 15% of all turns.
func (p *Planner) turn(i int) TurnPlan {
	t := TurnPlan{Think: p.between(time.Second, 3*time.Second)}
	barge := p.hasBarge && i > 0 && p.rng.Float64() < pBargeIn
	pf := pFiller
	if i > 0 && p.hasBarge {
		pf = pFiller / (1 - pBargeIn)
	}
	switch {
	case barge:
		t.Fixture = BargeInFixture
		t.BargeInAfter = p.between(time.Second, 2*time.Second)
	case len(p.filler) > 0 && p.rng.Float64() < pf:
		t.Fixture = p.filler[p.rng.IntN(len(p.filler))].Name
		t.Filler = true
	default:
		t.Fixture = p.speech[p.rng.IntN(len(p.speech))].Name
	}
	return t
}

// Steps turns the plan into a client script. pre maps a fixture name to its pre-encoded packets.
//
//   - AI_FIRST starts with WaitAIDone: the AI speaks first, and speaking at once would take its
//     greeting for the answer.
//   - Before an utterance: WaitAIDone and the turn's Think. A barge-in turn waits for the AI's audio
//     itself, so nothing precedes it.
//   - The end: WaitAIDone, then End (EndAfter > 0) or a Think beyond the session length, which the
//     session's expiry cuts short.
func (s SessionPlan) Steps(pre map[string][][]byte) []kotokiclient.Step {
	var steps []kotokiclient.Step
	if s.StartMode == "AI_FIRST" {
		steps = append(steps, kotokiclient.WaitAIDone{})
	}
	for i, t := range s.Turns {
		switch {
		case i > 0 && t.BargeInAfter > 0:
			steps = append(steps, kotokiclient.BargeIn{After: t.BargeInAfter, Packets: pre[BargeInFixture]})
		default:
			if i > 0 {
				steps = append(steps, kotokiclient.WaitAIDone{})
			}
			if i > 0 || s.StartMode == "AI_FIRST" {
				steps = append(steps, kotokiclient.Think{D: t.Think})
			}
			steps = append(steps, kotokiclient.Speak{Packets: pre[t.Fixture], Label: t.Fixture})
		}
		if s.Drop != nil && s.Drop.AfterTurn == i {
			steps = append(steps, kotokiclient.Drop{For: s.Drop.For})
		}
	}
	steps = append(steps, kotokiclient.WaitAIDone{})
	if s.EndAfter > 0 {
		return append(steps, kotokiclient.End{})
	}
	return append(steps, kotokiclient.Think{D: time.Duration(s.DurationSeconds)*time.Second + ExpiryMargin})
}
