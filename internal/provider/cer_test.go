package provider_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/testutil/livekit"
)

// TestCER pins the metric of the live STT tests (M9-T16); it has no live tag so that `make test`
// guards it. CER = rune Levenshtein distance / reference length after dropping spaces and
// 。、？！?!,.・「」.
func TestCER(t *testing.T) {
	require.InDelta(t, 0.2, livekit.CER("こんにちは", "こんにちわ"), 1e-9, "one substitution in five runes")
	require.Zero(t, livekit.CER("こんにちは", "こんにちは"))
	require.Zero(t, livekit.CER("こんにちは。", "こんにちは"), "。 is ignored")
	require.Zero(t, livekit.CER("こん にち は", "こんにちは"), "spaces are ignored")
	require.Zero(t, livekit.CER("「こんにちは、」？！?!,.・", "こんにちは"), "all listed punctuation is ignored")
	require.InDelta(t, 0.2, livekit.CER("こんにち", "こんにちは"), 1e-9, "one deletion")
	require.InDelta(t, 0.2, livekit.CER("こんにちはは", "こんにちは"), 1e-9, "one insertion")
	require.InDelta(t, 1.0, livekit.CER("あいうえお", "かきくけこ"), 1e-9)
	require.InDelta(t, 1.5, livekit.CER("あいうえおか", "きくけこ"), 1e-9, "can exceed 1: 6 edits over 4 reference runes")
	require.Zero(t, livekit.CER("", ""))
	require.InDelta(t, 1.0, livekit.CER("あ", ""), 1e-9, "empty reference")
	require.InDelta(t, 1.0, livekit.CER("", "あい"), 1e-9, "empty hypothesis")
}
