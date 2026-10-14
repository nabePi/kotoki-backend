package prompt

import (
	"strings"
	"text/template"
)

// funcMap is the fixed set of template functions (spec 6.8.1); eq/ne/if/range are built in.
var funcMap = template.FuncMap{
	"join":    strings.Join,
	"bullets": bullets,
	"quote":   func(s string) string { return "「" + s + "」" },
	"vocab":   vocab,
}

// bullets renders "- a\n- b"; an empty slice is "".
func bullets(items []string) string {
	lines := make([]string, len(items))
	for i, it := range items {
		lines[i] = "- " + it
	}
	return strings.Join(lines, "\n")
}

// vocab renders one "- 経験（けいけん）: experience" line per hint.
func vocab(hints []VocabHint) string {
	lines := make([]string, len(hints))
	for i, h := range hints {
		lines[i] = "- " + h.TermJA + "（" + h.Reading + "）: " + h.Meaning
	}
	return strings.Join(lines, "\n")
}
