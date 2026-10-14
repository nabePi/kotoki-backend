package prompt

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	terminators = "。！？!?\n"
	closers     = "」』）)\"'”’】"
	softBreaks  = "、,，  "
)

// Sentence is one speakable sentence of an LLM answer (D-014: no expression).
type Sentence struct {
	Index int
	Text  string
}

// SentenceParser cuts the plain-Japanese stream of an LLM into sentences and sanitises it for
// TTS and the transcript (spec 7.7.3). `<` and `>` are ordinary text: there are no tags.
type SentenceParser struct {
	buf  []rune // sanitised text not cut yet
	idx  int    // next sentence index
	hold string // 1-rune sentences waiting to be merged into the next one

	lastSpace bool // the last sanitised rune was a space (collapses runs across deltas)

	MaxRunes     int  // 60: hard cut when no terminator
	SoftMin      int  // 20: earliest soft boundary used
	MaxSentences int  // R-17: hard limit of emitted sentences (≥ 1)
	Limited      bool // true once the MaxSentences-th sentence was emitted; the parser then emits nothing
}

// NewSentenceParser returns a parser limited to maxSentences sentences (values < 1 mean 1).
func NewSentenceParser(maxSentences int) *SentenceParser {
	return &SentenceParser{MaxRunes: 60, SoftMin: 20, MaxSentences: max(maxSentences, 1)}
}

func isTerminator(r rune) bool { return strings.ContainsRune(terminators, r) }
func isCloser(r rune) bool     { return strings.ContainsRune(closers, r) }

// sanitize drops markdown characters (* # ` _), emoji (category So) and collapses runs of
// spaces; it returns the cleaned delta, which is also appended to buf.
func (p *SentenceParser) sanitize(delta string) []rune {
	out := make([]rune, 0, utf8.RuneCountInString(delta))
	for _, r := range delta {
		switch {
		case r == '*' || r == '#' || r == '`' || r == '_' || unicode.Is(unicode.So, r):
			continue
		case r == ' ':
			if p.lastSpace {
				continue
			}
			p.lastSpace = true
		default:
			p.lastSpace = false
		}
		out = append(out, r)
	}
	p.buf = append(p.buf, out...)
	return out
}

// Feed takes a raw LLM delta and returns the clean new text (for ai.text_delta and the
// transcript) and the sentences completed by it. After the sentence limit it returns nothing.
func (p *SentenceParser) Feed(delta string) (clean string, out []Sentence) {
	if p.Limited {
		return "", nil
	}
	cleaned := p.sanitize(delta)
	out = p.drain(false)
	if p.Limited {
		// the rest of the answer is discarded, including the part of this delta beyond the cut
		drop := min(len(p.buf), len(cleaned))
		cleaned = cleaned[:len(cleaned)-drop]
		p.buf = nil
	}
	return string(cleaned), out
}

// Close is called when the LLM stream ended normally: the remainder becomes the last sentence.
// It is not called when the generation was cancelled.
func (p *SentenceParser) Close() (clean string, out []Sentence) {
	if p.Limited {
		return "", nil
	}
	out = p.drain(true)
	if p.Limited {
		p.buf = nil
	}
	return "", out
}

func speakableRunes(s string) int { return utf8.RuneCountInString(NormalizeSpeakable(s)) }

// drain cuts every sentence available now, applying the merge/attach rules (rule 5) and the
// sentence limit (R-17).
func (p *SentenceParser) drain(final bool) []Sentence {
	var out []Sentence
	emit := func(text string) {
		out = append(out, Sentence{Index: p.idx, Text: text})
		p.idx++
		if p.idx >= p.MaxSentences {
			p.Limited = true // Feed/Close discard what is left in buf
			p.hold = ""
		}
	}
	for !p.Limited {
		seg, ok := p.cut(final)
		if !ok {
			break
		}
		seg = strings.TrimSpace(seg)
		switch n := speakableRunes(seg); {
		case n == 0:
			// punctuation only: never spoken, kept for the transcript of the previous sentence
			if len(out) > 0 {
				out[len(out)-1].Text += seg
			} else if p.hold != "" {
				p.hold += seg
			}
		case n == 1 && !final:
			p.hold += seg // too short to speak alone: merged into the next sentence
		default:
			emit(p.hold + seg)
			p.hold = ""
		}
	}
	if final && !p.Limited && p.hold != "" {
		emit(p.hold)
		p.hold = ""
	}
	return out
}

// cut removes and returns the next raw segment from buf (spec 7.7.3 rules 1–4).
func (p *SentenceParser) cut(final bool) (string, bool) {
	// 1. drop leading whitespace
	start := 0
	for start < len(p.buf) && unicode.IsSpace(p.buf[start]) {
		start++
	}
	p.buf = p.buf[start:]
	if len(p.buf) == 0 {
		return "", false
	}
	// 2. first terminator within the first MaxRunes runes (a sentence cannot start its
	// terminator beyond the hard limit, so feeding rune by rune and all at once agree)
	limit := min(len(p.buf), p.MaxRunes)
	for i := 0; i < limit; i++ {
		if !isTerminator(p.buf[i]) {
			continue
		}
		j := i
		for j+1 < len(p.buf) && (isTerminator(p.buf[j+1]) || isCloser(p.buf[j+1])) {
			j++
		}
		if j == len(p.buf)-1 && !final {
			return "", false // more terminators or closers may follow
		}
		return p.take(j + 1), true
	}
	// 3. no terminator: soft boundary, else hard cut, once MaxRunes are buffered
	if len(p.buf) >= p.MaxRunes {
		for k := p.MaxRunes - 1; k >= p.SoftMin; k-- {
			if strings.ContainsRune(softBreaks, p.buf[k]) {
				return p.take(k + 1), true
			}
		}
		return p.take(p.MaxRunes), true
	}
	// 4. end of the stream: everything left is the last sentence
	if final {
		return p.take(len(p.buf)), true
	}
	return "", false
}

func (p *SentenceParser) take(n int) string {
	s := string(p.buf[:n])
	p.buf = p.buf[n:]
	return s
}
