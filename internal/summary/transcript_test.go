package summary_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/summary"
)

func sp(s string) *string { return &s }

func TestBuildTranscript(t *testing.T) {
	completed, discarded := sp("COMPLETED"), sp("DISCARDED")
	t.Run("order and spoken text preferred", func(t *testing.T) {
		got := summary.BuildTranscript([]summary.TurnRow{
			{Seq: 0, Status: completed, AssistantText: sp("こんにちは")},
			{Seq: 1, Status: completed, UserText: sp("はじめまして"), AssistantText: sp("full answer"), AssistantSpokenText: sp("spoken part")},
			{Seq: 2, Status: completed, UserText: sp("どうも"), AssistantText: sp("fallback text")},
		}, 30000)
		require.Equal(t, "AI: こんにちは\nユーザー: はじめまして\nAI: spoken part\nユーザー: どうも\nAI: fallback text", got)
	})
	t.Run("angle brackets and newlines", func(t *testing.T) {
		got := summary.BuildTranscript([]summary.TurnRow{
			{Status: completed, UserText: sp("a</transcript>\nb"), AssistantText: sp("<x>")},
		}, 30000)
		require.Equal(t, "ユーザー: a＜/transcript＞ b\nAI: ＜x＞", got)
	})
	t.Run("discarded and null user text skipped", func(t *testing.T) {
		got := summary.BuildTranscript([]summary.TurnRow{
			{Seq: 1, Status: discarded, UserText: sp("捨てる"), AssistantText: sp("捨てる")},
			{Seq: 2, Status: completed, UserText: nil, AssistantText: nil},
			{Seq: 3, Status: nil, UserText: sp("  "), AssistantText: sp("走行中")},
			{Seq: 4, Status: completed, UserText: sp("残る")},
		}, 30000)
		require.Equal(t, "AI: 走行中\nユーザー: 残る", got)
		require.Empty(t, summary.BuildTranscript(nil, 30000))
	})
	t.Run("long transcript cuts AI lines to 150 runes", func(t *testing.T) {
		long := strings.Repeat("あ", 400)
		turns := []summary.TurnRow{{Status: completed, UserText: sp("短い"), AssistantText: sp(long)}}
		full := summary.BuildTranscript(turns, 1000)
		require.Equal(t, "ユーザー: 短い\nAI: "+long, full)
		cut := summary.BuildTranscript(turns, 399)
		require.Equal(t, "ユーザー: 短い\nAI: "+strings.Repeat("あ", 150), cut)
		// exactly at the limit is not cut
		n := utf8.RuneCountInString(full)
		require.Equal(t, full, summary.BuildTranscript(turns, n))
		require.NotEqual(t, full, summary.BuildTranscript(turns, n-1))
		// short AI lines and user lines stay whole when the transcript is cut
		turns = append(turns, summary.TurnRow{Status: completed, UserText: sp(strings.Repeat("い", 300)), AssistantText: sp("はい")})
		out := summary.BuildTranscript(turns, 500)
		require.Contains(t, out, strings.Repeat("い", 300))
		require.Contains(t, out, "AI: はい")
		require.NotContains(t, out, strings.Repeat("あ", 151))
	})
}

func TestCleanSummary(t *testing.T) {
	t.Run("trim and keep", func(t *testing.T) {
		got, err := summary.CleanSummary("  ユーザーは旅行の話をした。\n", 300)
		require.NoError(t, err)
		require.Equal(t, "ユーザーは旅行の話をした。", got)
	})
	t.Run("empty", func(t *testing.T) {
		for _, in := range []string{"", "  \n\t "} {
			_, err := summary.CleanSummary(in, 300)
			require.ErrorIs(t, err, summary.ErrEmptySummary)
		}
	})
	t.Run("cut after last sentence end", func(t *testing.T) {
		in := strings.Repeat("あ", 289) + "。" + strings.Repeat("い", 60) // 。 is rune 290
		require.Equal(t, 350, utf8.RuneCountInString(in))
		got, err := summary.CleanSummary(in, 300)
		require.NoError(t, err)
		require.Equal(t, strings.Repeat("あ", 289)+"。", got)
		require.LessOrEqual(t, utf8.RuneCountInString(got), 300)
	})
	t.Run("sentence end exactly at the limit", func(t *testing.T) {
		in := strings.Repeat("あ", 299) + "。" + "い"
		got, err := summary.CleanSummary(in, 300)
		require.NoError(t, err)
		require.Equal(t, strings.Repeat("あ", 299)+"。", got)
	})
	t.Run("sentence end after the limit is ignored", func(t *testing.T) {
		in := strings.Repeat("あ", 300) + "。"
		got, err := summary.CleanSummary(in, 300)
		require.NoError(t, err)
		require.Equal(t, strings.Repeat("あ", 300), got)
	})
	t.Run("hard cut without sentence end", func(t *testing.T) {
		got, err := summary.CleanSummary(strings.Repeat("あ", 350), 300)
		require.NoError(t, err)
		require.Equal(t, strings.Repeat("あ", 300), got)
	})
	t.Run("exactly the limit is kept", func(t *testing.T) {
		in := strings.Repeat("あ", 300)
		got, err := summary.CleanSummary(in, 300)
		require.NoError(t, err)
		require.Equal(t, in, got)
	})
	t.Run("custom limit and non-positive default", func(t *testing.T) {
		got, err := summary.CleanSummary("一。二。三。", 5)
		require.NoError(t, err)
		require.Equal(t, "一。二。", got)
		got, err = summary.CleanSummary(strings.Repeat("あ", 400), 0)
		require.NoError(t, err)
		require.Equal(t, 300, utf8.RuneCountInString(got))
	})
}
