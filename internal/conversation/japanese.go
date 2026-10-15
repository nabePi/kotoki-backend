package conversation

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/nabePi/kotoki-backend/internal/prompt"
)

// tailTrim is what NormalizeTail strips from the end besides whitespace (spec 7.6.2).
const tailTrim = "、,…ー〜～"

// fillerTokens are the long filler tokens of IsFillerOnly (spec 7.6.2), longest first so a
// shorter token never eats part of a longer one.
var fillerTokens = []string{"えっと", "ええと", "なんか", "うーん", "あの", "その", "まあ", "えー", "んー"}

// NormalizeTail repeatedly removes trailing whitespace (including U+3000) and the characters
// 、,…ー〜～: "えっとー" → "えっと", "あのー…" → "あの".
func NormalizeTail(s string) string {
	return strings.TrimRightFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune(tailTrim, r)
	})
}

// HasSuffixAny reports whether NormalizeTail(s) ends with one of the literal suffixes; an empty
// suffix never matches.
func HasSuffixAny(s string, suffixes []string) bool {
	t := NormalizeTail(s)
	for _, suf := range suffixes {
		if suf != "" && strings.HasSuffix(t, suf) {
			return true
		}
	}
	return false
}

// IsFillerOnly reports whether s holds at least one speakable rune and nothing is left once the
// long filler tokens, punctuation, symbols, whitespace and the lengthening marks ー〜～ are
// removed ("えっと、あの" → true, "えっと、昨日" → false).
func IsFillerOnly(s string) bool {
	t := prompt.NormalizeSpeakable(s)
	if t == "" {
		return false
	}
	for _, tok := range fillerTokens {
		t = strings.ReplaceAll(t, tok, "")
	}
	return strings.Trim(t, "ー〜～") == ""
}

// RuneLen is the number of runes in s.
func RuneLen(s string) int { return utf8.RuneCountInString(s) }

// isDiscard decides whether the text of a finished user turn is noise (spec 7.6.2, transition
// table row 8): no speakable rune at all, or at most one rune from under 400 ms of voice.
// Filler-only utterances are kept.
func isDiscard(text string, voicedMs int64) bool {
	t := prompt.NormalizeSpeakable(text)
	if RuneLen(t) == 0 {
		return true // no word: cough, breath, noise
	}
	if voicedMs < 400 && RuneLen(t) <= 1 {
		return true // short "ん", "あ" → noise
	}
	return false
}

// HasJapanese reports whether s holds a Hiragana, Katakana or Han rune.
func HasJapanese(s string) bool {
	for _, r := range s {
		if unicode.In(r, unicode.Hiragana, unicode.Katakana, unicode.Han) {
			return true
		}
	}
	return false
}
