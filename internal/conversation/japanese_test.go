package conversation

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestJapaneseNormalizeTail(t *testing.T) {
	for in, want := range map[string]string{
		"えっとー":    "えっと",
		"あのー…":    "あの",
		"けど、 　":   "けど",
		"そう〜～,":   "そう",
		"ですか。":    "ですか。",
		"ー、…":     "",
		"":        "",
		"日本語です？":  "日本語です？",
		"\tまあー\n": "\tまあ",
	} {
		require.Equal(t, want, NormalizeTail(in), "input %q", in)
	}
}

func TestJapaneseHasSuffixAny(t *testing.T) {
	sfx := []string{"か", "です", "。"}
	require.True(t, HasSuffixAny("ですか", sfx))
	require.True(t, HasSuffixAny("学生です、", sfx), "suffix is matched after NormalizeTail")
	require.True(t, HasSuffixAny("はい。", sfx))
	require.False(t, HasSuffixAny("ですが", sfx))
	require.False(t, HasSuffixAny("", sfx))
	require.False(t, HasSuffixAny("ですか", nil))
	require.False(t, HasSuffixAny("ですか", []string{""}), "an empty suffix never matches")
}

func TestJapaneseIsFillerOnly(t *testing.T) {
	for in, want := range map[string]bool{
		"えっと":         true,
		"えっと、あの":      true,
		"ええと…まあ":      true,
		"うーん、えー。":     true,
		"んー":          true,
		"あのー、えっと":     true,
		"なんか その":      true,
		"えっと、昨日":      false,
		"ですが":         false,
		"":            false,
		"、。":          false,
		"あの人":         false,
		"その本はおもしろかった": false,
	} {
		require.Equal(t, want, IsFillerOnly(in), "input %q", in)
	}
}

func TestJapaneseRuneLen(t *testing.T) {
	require.Equal(t, 0, RuneLen(""))
	require.Equal(t, 2, RuneLen("あの"))
	require.Equal(t, 4, RuneLen("OKです"))
}

func TestDiscard(t *testing.T) {
	cases := []struct {
		text     string
		voicedMs int64
		want     bool
	}{
		{"", 1000, true},
		{"、。 ", 1000, true},
		{"ん", 300, true},
		{"ん", 399, true},
		{"ん", 400, false},
		{"あ。", 300, true},
		{"はい", 100, false},
		{"えっと", 300, false}, // filler-only utterances are not discarded
	}
	for _, c := range cases {
		require.Equal(t, c.want, isDiscard(c.text, c.voicedMs), "isDiscard(%q, %d)", c.text, c.voicedMs)
	}
}

func TestJapaneseHasJapanese(t *testing.T) {
	require.False(t, HasJapanese("OK"))
	require.False(t, HasJapanese(""))
	require.False(t, HasJapanese("123!?"))
	require.True(t, HasJapanese("OKです"))
	require.True(t, HasJapanese("カタカナ"))
	require.True(t, HasJapanese("漢字"))
}
