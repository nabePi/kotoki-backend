package kotokiclient

import "time"

// Step is one instruction of a conversation script (§14.11.1). The set is closed: Speak,
// WaitAIDone, Think, BargeIn, Drop and End. While the script runs the client keeps the upstream
// open: every 20 ms it sends the next packet of the current step, or a silence packet when the
// step has nothing to say (a mic that is open but quiet, D-071).
type Step interface{ isStep() }

// Speak sends Packets, one per 20 ms slot, as one utterance. The server decides when the turn
// ends (D-076), from the silence that follows, so a Speak is normally followed by a WaitAIDone
// or a Think. Label is only for messages.
type Speak struct {
	Packets [][]byte
	Label   string
}

// WaitAIDone sends silence until the AI has answered the last utterance: a turn.completed
// arrived for a turn after it (completed, interrupted, failed or discarded alike). It fails
// with ErrAITimeout after Timeout (zero means DefaultAITimeout).
type WaitAIDone struct{ Timeout time.Duration }

// Think sends silence for D.
type Think struct{ D time.Duration }

// BargeIn interrupts the AI: it sends silence until the first audio frame of the AI's answer to
// the last utterance has arrived, waits After more (silence), then sends Packets as a new
// utterance on top of the AI's speech. Timeout bounds the wait for that first frame (zero means
// DefaultAITimeout); it fails with ErrNoAIAudio if the AI's turn ends without audio.
type BargeIn struct {
	After   time.Duration
	Packets [][]byte
	Timeout time.Duration
}

// Drop breaks the connection like a network failure (the transport is closed, no close
// handshake), stays away for For, then reconnects with a fresh ticket and session.resume
// carrying the last_server_seq the client has seen, and continues with the next step.
type Drop struct{ For time.Duration }

// End sends session.end{USER_ENDED} and keeps the upstream open until the server has completed
// the session. It fails with ErrEndTimeout when that takes longer than Timeout (zero means
// DefaultEndTimeout).
type End struct{ Timeout time.Duration }

func (Speak) isStep()      {}
func (WaitAIDone) isStep() {}
func (Think) isStep()      {}
func (BargeIn) isStep()    {}
func (Drop) isStep()       {}
func (End) isStep()        {}

func stepName(s Step) string {
	switch st := s.(type) {
	case Speak:
		if st.Label != "" {
			return "Speak " + st.Label
		}
		return "Speak"
	case WaitAIDone:
		return "WaitAIDone"
	case Think:
		return "Think"
	case BargeIn:
		return "BargeIn"
	case Drop:
		return "Drop"
	case End:
		return "End"
	default:
		return "unknown"
	}
}
