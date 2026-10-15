package provider

import (
	"strings"
	"unicode/utf8"
)

const (
	combiningVoiced     = '゙'
	combiningSemiVoiced = '゚'
)

// composeKana returns the precomposed kana for base + a voiced (dakuten) or semi-voiced
// (handakuten) combining mark, or 0 when no such character exists.
func composeKana(base, mark rune) rune {
	for _, off := range []rune{0, 0x60} { // hiragana, katakana
		b := base - off
		switch mark {
		case combiningVoiced:
			switch {
			case b == 'う':
				return 'ゔ' + off
			case b == 'ゝ':
				return 'ゞ' + off
			case isVoicedBase(b):
				return base + 1
			}
			if off != 0 {
				switch base {
				case 'ワ':
					return 'ヷ'
				case 'ヰ':
					return 'ヸ'
				case 'ヱ':
					return 'ヹ'
				case 'ヲ':
					return 'ヺ'
				case 'ヽ':
					return 'ヾ'
				}
			}
		case combiningSemiVoiced:
			switch b {
			case 'は', 'ひ', 'ふ', 'へ', 'ほ':
				return base + 2
			}
		}
	}
	return 0
}

func isVoicedBase(b rune) bool {
	switch b {
	case 'か', 'き', 'く', 'け', 'こ', 'さ', 'し', 'す', 'せ', 'そ', 'た', 'ち', 'つ', 'て', 'と', 'は', 'ひ', 'ふ', 'へ', 'ほ':
		return true
	}
	return false
}

func isASCII(r rune) bool { return r < utf8.RuneSelf }

func isSpace(r rune) bool { return r == ' ' || r == '　' }

// NormalizeJA applies the text normalisation every adapter must use (spec 8.4.0): NFC
// composition of kana + dakuten/handakuten (no x/text dependency, D-003), removal of
// U+0020/U+3000 between two non-ASCII runes, and trimming.
func NormalizeJA(s string) string {
	// 1. compose kana + combining marks
	in := []rune(s)
	composed := make([]rune, 0, len(in))
	for i := 0; i < len(in); i++ {
		if i+1 < len(in) && (in[i+1] == combiningVoiced || in[i+1] == combiningSemiVoiced) {
			if c := composeKana(in[i], in[i+1]); c != 0 {
				composed = append(composed, c)
				i++
				continue
			}
		}
		composed = append(composed, in[i])
	}
	// 2. drop space runs flanked by non-ASCII runes
	var b strings.Builder
	for i := 0; i < len(composed); i++ {
		if !isSpace(composed[i]) {
			b.WriteRune(composed[i])
			continue
		}
		j := i
		for j < len(composed) && isSpace(composed[j]) {
			j++
		}
		if i > 0 && j < len(composed) && !isASCII(composed[i-1]) && !isASCII(composed[j]) {
			i = j - 1 // drop the whole run
			continue
		}
		for k := i; k < j; k++ {
			b.WriteRune(composed[k])
		}
		i = j - 1
	}
	return strings.TrimSpace(b.String())
}
