package provider_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/provider"
)

func TestNormalizeJA(t *testing.T) {
	require.Equal(t, "日本語の勉強", provider.NormalizeJA("日本語 の　勉強"))
	require.Equal(t, "Hello 日本", provider.NormalizeJA(" Hello 日本 ")) // ASCII kept, trimmed
	require.Equal(t, "が", provider.NormalizeJA("が"), "か + dakuten composes to が")
	require.Equal(t, "ぱ", provider.NormalizeJA("ぱ"))
	require.Equal(t, "ヴ", provider.NormalizeJA("ヴ"))
	require.Equal(t, "バナナ", provider.NormalizeJA("バナナ"))
	require.Equal(t, "こんにちは世界", provider.NormalizeJA("こんにちは 世界"), "a space between two Japanese runes goes")
}

func TestNormalizeJAKeepsSpaceNextToASCII(t *testing.T) {
	require.Equal(t, "iPhone を買う", provider.NormalizeJA("iPhone を買う"))
	require.Equal(t, "a b", provider.NormalizeJA("a b"))
}
