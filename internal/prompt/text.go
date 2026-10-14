package prompt

import (
	"strings"
	"unicode"
)

// NormalizeSpeakable removes whitespace and every punctuation (P*) and symbol (S*) rune, so
// two spoken-text variants compare equal. The sentence parser (Bab 7) uses it; the long
// vowel mark ー is a letter and stays.
func NormalizeSpeakable(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.IsSymbol(r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
