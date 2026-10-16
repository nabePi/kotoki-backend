package evaluation

import (
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	aiLineMaxRunes   = 200
	keepLastTurns    = 120
	omittedMarker    = "[earlier turns omitted]"
	noReplyText      = "(no reply: system error)"
	minEligibleRunes = 2
)

// TurnRow is one session_turns row as the evaluation needs it (spec 10.4, 10.6.2).
type TurnRow struct {
	ID                                           uuid.UUID
	CreatedAt                                    time.Time
	Seq                                          int
	Status                                       *string
	UserText, AssistantText, AssistantSpokenText *string
	UserStartedAt, UserEndedAt                   *time.Time
}

// EligibleUserTurn reports whether the turn counts as a user turn: status COMPLETED, INTERRUPTED
// or FAILED and user_text of at least two runes after trimming.
func EligibleUserTurn(t TurnRow) bool {
	if t.Status == nil || t.UserText == nil {
		return false
	}
	switch *t.Status {
	case "COMPLETED", "INTERRUPTED", "FAILED":
	default:
		return false
	}
	return utf8.RuneCountInString(strings.TrimSpace(*t.UserText)) >= minEligibleRunes
}

// userDuration is user_ended_at − user_started_at; 0 when either is NULL or the span is negative.
func userDuration(t TurnRow) time.Duration {
	if t.UserStartedAt == nil || t.UserEndedAt == nil {
		return 0
	}
	return max(0, t.UserEndedAt.Sub(*t.UserStartedAt))
}

// ComputeStats derives the eligibility statistics: user_speech_seconds is the floor of the summed
// durations of eligible turns, active_seconds is floor(ended − started) − disconnected, never
// negative (spec 10.4).
func ComputeStats(turns []TurnRow, startedAt, endedAt time.Time, disconnectedSeconds int) Stats {
	var st Stats
	var speech time.Duration
	for _, t := range turns {
		if EligibleUserTurn(t) {
			st.UserTurns++
			speech += userDuration(t)
		}
	}
	st.UserSpeechSeconds = int(speech / time.Second)
	st.ActiveSeconds = max(0, int(max(0, endedAt.Sub(startedAt))/time.Second)-disconnectedSeconds)
	return st
}

// block is one rendered turn: an optional USER line and an optional AI line.
type block struct {
	seq    int
	user   string // complete USER line; "" when the turn has none
	aiMark string // " (cut off by the user)" or ""
	aiText string // "" when the turn has no AI line
}

func (b block) lines(truncate bool) []string {
	var out []string
	if b.user != "" {
		out = append(out, b.user)
	}
	if b.aiText != "" {
		text := b.aiText
		if truncate {
			text = cutRunes(text, aiLineMaxRunes)
		}
		out = append(out, fmt.Sprintf("[T%d] AI%s: %s", b.seq, b.aiMark, text))
	}
	return out
}

func oneLine(s string) string {
	return strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ").Replace(strings.TrimSpace(s))
}

func nonSpaceRunes(s string) int {
	n := 0
	for _, r := range s {
		if !unicode.IsSpace(r) {
			n++
		}
	}
	return n
}

func cutRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}

func render(blocks []block, truncate bool) string {
	var lines []string
	for _, b := range blocks {
		lines = append(lines, b.lines(truncate)...)
	}
	return strings.Join(lines, "\n")
}

func turnBlock(t TurnRow, prevInterrupted bool) block {
	b := block{seq: t.Seq}
	if t.UserText != nil && strings.TrimSpace(*t.UserText) != "" {
		secs := userDuration(t).Seconds()
		cps := 0.0
		if secs > 0 {
			cps = float64(nonSpaceRunes(*t.UserText)) / secs
		}
		extra := ""
		if prevInterrupted {
			extra = ", interrupted the AI"
		}
		b.user = fmt.Sprintf("[T%d] USER (%.1f s, %.1f chars/s%s): %s", t.Seq, secs, cps, extra, oneLine(*t.UserText))
	}
	ai := ""
	switch {
	case t.AssistantSpokenText != nil:
		ai = *t.AssistantSpokenText
	case t.AssistantText != nil:
		ai = *t.AssistantText
	}
	b.aiText = oneLine(ai)
	if b.aiText == "" && *t.Status == "FAILED" {
		b.aiText = noReplyText
	}
	if b.aiText != "" && *t.Status == "INTERRUPTED" {
		b.aiMark = " (cut off by the user)"
	}
	return b
}

// BuildTranscript renders the transcript of spec 10.6.2: one block per turn (USER line, then AI
// line) in the given order. Turns whose status is NULL or DISCARDED are skipped; newlines inside
// turn text become spaces so a turn cannot forge another line. When the text exceeds maxChars runes
// every AI line is cut to 200 runes (USER lines never are); when it is still too long only the last
// 120 turns are kept behind an "[earlier turns omitted]" line. maxChars <= 0 disables both limits.
func BuildTranscript(turns []TurnRow, maxChars int) string {
	var blocks []block
	prevInterrupted := false
	for _, t := range turns {
		if t.Status == nil || *t.Status == "DISCARDED" {
			continue
		}
		blocks = append(blocks, turnBlock(t, prevInterrupted))
		prevInterrupted = *t.Status == "INTERRUPTED"
	}
	out := render(blocks, false)
	if maxChars <= 0 || utf8.RuneCountInString(out) <= maxChars {
		return out
	}
	out = render(blocks, true)
	if utf8.RuneCountInString(out) <= maxChars || len(blocks) <= keepLastTurns {
		return out
	}
	return omittedMarker + "\n" + render(blocks[len(blocks)-keepLastTurns:], true)
}
