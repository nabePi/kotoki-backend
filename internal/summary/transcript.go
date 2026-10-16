package summary

import (
	"errors"
	"strings"
	"unicode/utf8"
)

const (
	// TranscriptMaxChars is the transcript length above which AI lines are shortened (spec 9.9.6
	// step 3).
	TranscriptMaxChars = 30000
	// aiLineMaxRunes is the length each AI line is cut to once the transcript is too long.
	aiLineMaxRunes = 150
)

// ErrEmptySummary is returned by CleanSummary when the model produced no text; it is retryable.
var ErrEmptySummary = errors.New("summary: empty output")

// TurnRow is one session_turns row as the summary needs it (spec 9.9.6 step 3).
type TurnRow struct {
	Seq                                          int
	Status                                       *string
	UserText, AssistantText, AssistantSpokenText *string
}

var angleReplacer = strings.NewReplacer("<", "＜", ">", "＞", "\r\n", " ", "\n", " ", "\r", " ")

// clean makes a text safe for one transcript line: single line, no angle brackets (they could
// close the <transcript> tag of the prompt).
func clean(s string) string { return angleReplacer.Replace(strings.TrimSpace(s)) }

func cutRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// BuildTranscript renders "ユーザー: …" / "AI: …" lines in turn order. DISCARDED turns and empty
// texts are skipped; the AI line uses assistant_spoken_text and falls back to assistant_text when
// that is NULL. When the result exceeds maxChars runes every AI line is cut to 150 runes.
func BuildTranscript(turns []TurnRow, maxChars int) string {
	type line struct {
		ai   bool
		text string
	}
	var lines []line
	for _, t := range turns {
		if t.Status != nil && *t.Status == "DISCARDED" {
			continue
		}
		if t.UserText != nil {
			if s := clean(*t.UserText); s != "" {
				lines = append(lines, line{text: s})
			}
		}
		ai := t.AssistantSpokenText
		if ai == nil {
			ai = t.AssistantText
		}
		if ai != nil {
			if s := clean(*ai); s != "" {
				lines = append(lines, line{ai: true, text: s})
			}
		}
	}
	join := func(short bool) string {
		out := make([]string, len(lines))
		for i, l := range lines {
			switch {
			case !l.ai:
				out[i] = "ユーザー: " + l.text
			case short:
				out[i] = "AI: " + cutRunes(l.text, aiLineMaxRunes)
			default:
				out[i] = "AI: " + l.text
			}
		}
		return strings.Join(out, "\n")
	}
	full := join(false)
	if utf8.RuneCountInString(full) <= maxChars {
		return full
	}
	return join(true)
}

// CleanSummary trims the model output and enforces maxChars (default 300 when not positive): empty
// is ErrEmptySummary; a longer text is cut after the last "。" within the limit, or hard-cut at the
// limit when there is none.
func CleanSummary(text string, maxChars int) (string, error) {
	if maxChars <= 0 {
		maxChars = DefaultMaxChars
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return "", ErrEmptySummary
	}
	r := []rune(text)
	if len(r) <= maxChars {
		return text, nil
	}
	for i := maxChars - 1; i >= 0; i-- {
		if r[i] == '。' {
			return string(r[:i+1]), nil
		}
	}
	return string(r[:maxChars]), nil
}
