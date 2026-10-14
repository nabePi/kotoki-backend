package prompt_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/prompt"
)

func TestEstimateTokens(t *testing.T) {
	require.Equal(t, 0, prompt.EstimateTokens(""))
	require.Equal(t, 11, prompt.EstimateTokens(strings.Repeat("あ", 10))) // ceil(1.1×10): exact, no float drift
	require.Equal(t, 3, prompt.EstimateTokens("hello wor"))              // ceil(9/3.5) = ceil(2.57)
	require.Equal(t, 9, prompt.EstimateTokens("あいうえお"+"あ"+"abcdefg"))    // ceil(1.1×6 + 7/3.5) = ceil(8.6)
	require.Equal(t, 1, prompt.EstimateTokens("a"))
	require.Equal(t, 2, prompt.EstimateTokens("あ"), "ceil(1.1)")
	require.Equal(t, 3, prompt.EstimateTokens("あい"), "ceil(1.1×2) = ceil(2.2)")
}

func TestEstimateTokensBoundary(t *testing.T) {
	require.Equal(t, 1, prompt.EstimateTokens(string(rune(0x2fff))), "below U+3000 counts as other text")
	require.Equal(t, 2, prompt.EstimateTokens(string(rune(0x3000))), "U+3000 itself is CJK-class: ceil(1.1)")
	require.Equal(t, 2, prompt.EstimateTokens("ab　"), "2/3.5 + 1.1 = 1.67")
	require.Equal(t, 5, prompt.EstimateTokens("。。。。"), "CJK punctuation is CJK-class: ceil(4.4)")
}

func TestItemOverhead(t *testing.T) { require.Equal(t, 4, prompt.ItemOverheadTokens) }
