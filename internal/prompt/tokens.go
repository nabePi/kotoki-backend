// Package prompt renders prompt templates and holds the code-owned prompt text, the token
// estimator, the context budget and the sentence parser. It never imports a feature package
// (spec 2.2): callers pass plain values, so content can depend on prompt and not the reverse.
package prompt

// ItemOverheadTokens is added per list item (bullet + newline) when sizing list components
// (memory, Scenario.Tasks, ...).
const ItemOverheadTokens = 4

// cjkFloor is the first rune counted as CJK-class (full-width space, kana, kanji, full-width
// forms).
const cjkFloor = 0x3000

// EstimateTokens is the single token estimator of the system: ceil(1.1 × runes ≥ U+3000 +
// other runes / 3.5). It is computed in integers — (77·cjk + 20·other) / 70, rounded up — so
// float drift can never turn an exact 11.0 into 12.
func EstimateTokens(s string) int {
	cjk, other := 0, 0
	for _, r := range s {
		if r >= cjkFloor {
			cjk++
		} else {
			other++
		}
	}
	return (77*cjk + 20*other + 69) / 70
}
