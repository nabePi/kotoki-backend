package prompt_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/prompt"
)

func texts(ss []prompt.Sentence) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = s.Text
	}
	return out
}

// feedAll feeds the chunks, then closes, and returns every sentence in order.
func feedAll(p *prompt.SentenceParser, chunks ...string) []prompt.Sentence {
	var all []prompt.Sentence
	for _, c := range chunks {
		_, o := p.Feed(c)
		all = append(all, o...)
	}
	_, o := p.Close()
	return append(all, o...)
}

func TestCutOnTerminatorWithTrailingClosers(t *testing.T) {
	p := prompt.NewSentenceParser(6)
	_, o := p.Feed("「はい。」そうですか？！")
	require.Equal(t, []string{"「はい。」"}, texts(o), "the closer stays with its sentence")
	_, o2 := p.Feed("次")
	require.Equal(t, []string{"そうですか？！"}, texts(o2), "?! is one sentence and leaves after the next rune")
}

func TestMaxSentencesLimitsAndDiscardsRest(t *testing.T) {
	p := prompt.NewSentenceParser(2)
	_, o := p.Feed("一つ目です。二つ目です。三つ目です。")
	require.Len(t, o, 2)
	require.True(t, p.Limited)
	c, o2 := p.Feed("四つ目です。")
	require.Empty(t, c)
	require.Empty(t, o2)
	c, o3 := p.Close()
	require.Empty(t, c)
	require.Empty(t, o3)
}

func TestLimitDropsTheRestOfTheDeltaFromClean(t *testing.T) {
	p := prompt.NewSentenceParser(1)
	clean, o := p.Feed("一つ目です。二つ目です。")
	require.Equal(t, []string{"一つ目です。"}, texts(o))
	require.Equal(t, "一つ目です。", clean, "text after the last emitted sentence is discarded, also from the transcript")
}

func TestMinimumOneSentence(t *testing.T) {
	require.Equal(t, 1, prompt.NewSentenceParser(0).MaxSentences)
	require.Equal(t, 1, prompt.NewSentenceParser(-3).MaxSentences)
	p := prompt.NewSentenceParser(0)
	_, o := p.Feed("一つ目です。二つ目です。")
	require.Len(t, o, 1)
	require.Equal(t, 60, p.MaxRunes)
	require.Equal(t, 20, p.SoftMin)
}

func TestMarkdownAndEmojiStripped(t *testing.T) {
	p := prompt.NewSentenceParser(6)
	clean, _ := p.Feed("**はい** `です` # _ね_ 😀  そう")
	require.Equal(t, "はい です ね そう", clean, "markdown characters and emoji go, double spaces collapse")
	p2 := prompt.NewSentenceParser(6)
	c1, _ := p2.Feed("はい ")
	c2, _ := p2.Feed(" ええ")
	require.Equal(t, "はい ", c1)
	require.Equal(t, "ええ", c2, "a space run is collapsed across deltas too")
}

func TestAngleBracketsAreLiteral(t *testing.T) {
	p := prompt.NewSentenceParser(6)
	clean, o := p.Feed("<e:happy>はい。次")
	require.Equal(t, "<e:happy>はい。次", clean)
	require.Equal(t, []string{"<e:happy>はい。"}, texts(o), "there are no tags in the system: < and > are ordinary text")
}

func TestFeedCleanEqualsSanitizedText(t *testing.T) {
	p := prompt.NewSentenceParser(6)
	var b strings.Builder
	for _, d := range []string{"こん", "にち*は。", "元気 ", "です_か？"} {
		c, _ := p.Feed(d)
		b.WriteString(c)
	}
	require.Equal(t, "こんにちは。元気 ですか？", b.String())
}

func TestWaitsWhenTerminatorIsLastRune(t *testing.T) {
	p := prompt.NewSentenceParser(6)
	_, o := p.Feed("はい。")
	require.Empty(t, o, "a closer or another terminator may still follow")
	_, o = p.Close()
	require.Equal(t, []string{"はい。"}, texts(o))
}

func TestHardCutAt60Runes(t *testing.T) {
	p := prompt.NewSentenceParser(6)
	text := strings.Repeat("あ", 62)
	_, o := p.Feed(text)
	require.Len(t, o, 1)
	require.Equal(t, 60, utf8.RuneCountInString(o[0].Text))
	_, o = p.Close()
	require.Equal(t, []string{"ああ"}, texts(o))
}

func TestSoftBoundaryCutAtOrAfter20(t *testing.T) {
	p := prompt.NewSentenceParser(6)
	text := strings.Repeat("あ", 29) + "、" + strings.Repeat("い", 40) // boundary at index 29
	_, o := p.Feed(text)
	require.Len(t, o, 1)
	require.Equal(t, strings.Repeat("あ", 29)+"、", o[0].Text, "cut right after the soft boundary")

	p2 := prompt.NewSentenceParser(6)
	early := strings.Repeat("あ", 9) + "、" + strings.Repeat("い", 60) // boundary at index 9 < SoftMin
	_, o = p2.Feed(early)
	require.Len(t, o, 1)
	require.Equal(t, 60, utf8.RuneCountInString(o[0].Text), "a boundary before SoftMin is not used: hard cut")

	p3 := prompt.NewSentenceParser(6)
	late := strings.Repeat("あ", 25) + "、" + strings.Repeat("い", 10) + "，" + strings.Repeat("う", 30) // two boundaries
	_, o = p3.Feed(late)
	require.Equal(t, strings.Repeat("あ", 25)+"、"+strings.Repeat("い", 10)+"，", o[0].Text, "the last boundary within 60 runes wins")
}

func TestPunctuationOnlySentenceAppendedToPrevious(t *testing.T) {
	p := prompt.NewSentenceParser(6)
	o := feedAll(p, "はい。\n。")
	require.Len(t, o, 1)
	require.Contains(t, o[0].Text, "はい。")
	require.Equal(t, "はい。\n。", o[0].Text)

	p2 := prompt.NewSentenceParser(6)
	_, first := p2.Feed("はい。次です。")
	require.Equal(t, []string{"はい。"}, texts(first))
	_, o = p2.Feed("「」。ええ。")
	require.Equal(t, []string{"次です。「」。"}, texts(o), "a punctuation-only segment is attached to the previous sentence, never a sentence of its own")
	require.Equal(t, 1, o[0].Index)
}

func TestSingleRuneSentenceMergedIntoNext(t *testing.T) {
	p := prompt.NewSentenceParser(6)
	o := feedAll(p, "あ。いいですね。")
	require.Equal(t, []string{"あ。いいですね。"}, texts(o))

	p2 := prompt.NewSentenceParser(6)
	o = feedAll(p2, "はい。あ")
	require.Equal(t, []string{"はい。", "あ"}, texts(o), "at the end of the stream a 1-rune sentence stands alone")

	p3 := prompt.NewSentenceParser(6)
	o = feedAll(p3, "あ。")
	require.Equal(t, []string{"あ。"}, texts(o), "held until the end, then emitted alone")
}

func TestNewlineIsTerminator(t *testing.T) {
	p := prompt.NewSentenceParser(6)
	o := feedAll(p, "一行目です\n二行目です")
	require.Equal(t, []string{"一行目です", "二行目です"}, texts(o))
}

func TestIndexIncrementsPerEmittedSentence(t *testing.T) {
	p := prompt.NewSentenceParser(6)
	o := feedAll(p, "あ。いいですね。", "うれしいです。次です。")
	require.Len(t, o, 3)
	for i, s := range o {
		require.Equal(t, i, s.Index)
	}
}

func TestLimitedCountsEmittedNotRawTerminators(t *testing.T) {
	p := prompt.NewSentenceParser(2)
	_, o := p.Feed("あ。いいですね。うれしいです。")
	require.Equal(t, []string{"あ。いいですね。"}, texts(o), "the merged 1-rune sentence counts once")
	require.False(t, p.Limited)
	_, o = p.Close()
	require.Equal(t, []string{"うれしいです。"}, texts(o))
	require.True(t, p.Limited)
}

func TestFeedByRuneEqualsFeedWhole(t *testing.T) {
	samples := []string{
		"こんにちは。今日はいい天気ですね。散歩に行きませんか？",
		"「はい。」そうですか？！へえ、すごいですね！",
		"あ。いいですね。うれしいです。\n。次の話です",
		strings.Repeat("あ", 25) + "、" + strings.Repeat("い", 40) + "。終わり",
		strings.Repeat("あ", 70) + "。" + strings.Repeat("い", 61),
		"**はい**、`そう`です_ね_ 😀 次の文です。最後の文",
	}
	for _, s := range samples {
		whole := feedAll(prompt.NewSentenceParser(10), s)
		var perRune []string
		p := prompt.NewSentenceParser(10)
		var sentences []prompt.Sentence
		for _, r := range s {
			_, o := p.Feed(string(r))
			sentences = append(sentences, o...)
		}
		_, o := p.Close()
		sentences = append(sentences, o...)
		perRune = texts(sentences)
		require.Equal(t, texts(whole), perRune, s)
		for i, sn := range sentences {
			require.Equal(t, i, sn.Index)
		}
	}
}

func TestCloseEmitsRemainderAsLastSentence(t *testing.T) {
	p := prompt.NewSentenceParser(6)
	_, o := p.Feed("一つ目です。続きの文")
	require.Equal(t, []string{"一つ目です。"}, texts(o))
	_, o = p.Close()
	require.Equal(t, []string{"続きの文"}, texts(o))
}

func TestCancelledParserNotClosed(t *testing.T) {
	p := prompt.NewSentenceParser(6)
	clean, o := p.Feed("一つ目です。途中の文")
	require.Equal(t, "一つ目です。途中の文", clean, "the fragment is in the transcript text")
	require.Equal(t, []string{"一つ目です。"}, texts(o), "but it is not a sentence: it is never spoken")
}

func TestLeadingWhitespaceAndEmptyInput(t *testing.T) {
	p := prompt.NewSentenceParser(6)
	_, o := p.Feed("   \n  ")
	require.Empty(t, o)
	_, o = p.Close()
	require.Empty(t, o)
	p2 := prompt.NewSentenceParser(6)
	require.Equal(t, []string{"はい。"}, texts(feedAll(p2, "\n  はい。")))
}

func TestClosersExtendTheSentence(t *testing.T) {
	for in, want := range map[string]string{
		"彼は「行く。」と言った。": "彼は「行く。」",
		"（はい。）次":       "（はい。）",
		"“はい。”次":       "“はい。”",
	} {
		p := prompt.NewSentenceParser(6)
		_, o := p.Feed(in)
		require.NotEmpty(t, o, in)
		require.Equal(t, want, o[0].Text, in)
	}
}
