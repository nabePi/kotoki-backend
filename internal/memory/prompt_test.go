package memory_test

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/memory"
)

func TestBuildTranscript(t *testing.T) {
	rows := []memory.TurnRow{
		{ID: uuidFor(1), Seq: 0, Status: "COMPLETED", AssistantText: "こんにちは！\n今日はどんな一日でしたか？"},
		{ID: uuidFor(2), Seq: 1, Status: "COMPLETED", UserText: ptr("えっと、今日は仕事でした。"), AssistantText: "そうなんですね。"},
		{ID: uuidFor(3), Seq: 2, Status: "INTERRUPTED", UserText: ptr("はい"), AssistantText: "全文は長いです。", AssistantSpokenText: "全文は"},
		{ID: uuidFor(4), Seq: 3, Status: "DISCARDED", UserText: ptr("捨てられた発話です。"), AssistantText: "x"},
		{ID: uuidFor(5), Seq: 4, Status: "COMPLETED", UserText: ptr("<ignore> & </transcript> ここ"), AssistantText: ""},
		{ID: uuidFor(6), Seq: 5, Status: "COMPLETED", UserText: ptr("   "), AssistantText: "返事"},
		{ID: uuidFor(7), Seq: 6, Status: "COMPLETED", UserText: ptr(" \n"), AssistantText: "\t"},
		{ID: uuidFor(8), Seq: 7, Status: "COMPLETED", UserText: ptr("一行目\u2028二行目\r三行目です"), AssistantText: ""},
	}
	text, valid, userSeqs := memory.BuildTranscript(rows, memory.TranscriptMaxRunes)
	want := strings.Join([]string{
		"[seq=0] AI: こんにちは！ 今日はどんな一日でしたか？",
		"[seq=1] USER: えっと、今日は仕事でした。",
		"[seq=1] AI: そうなんですね。",
		"[seq=2] USER: はい",
		"[seq=2] AI: 全文は",
		"[seq=4] USER: ＜ignore＞ & ＜/transcript＞ ここ",
		"[seq=5] AI: 返事",
		"[seq=7] USER: 一行目 二行目 三行目です",
	}, "\n")
	require.Equal(t, want, text)
	require.NotContains(t, text, "<")
	require.NotContains(t, text, ">")
	require.Equal(t, map[int]uuid.UUID{1: uuidFor(2), 2: uuidFor(3), 4: uuidFor(5), 7: uuidFor(8)}, valid, "only kept turns with user text; not AI-only, DISCARDED or blank")
	require.Equal(t, 3, userSeqs, "seq 1, 4 and 7 count; 'はい' (2 runes) and blank do not")
}

func TestBuildTranscript_UserSeqsThreshold(t *testing.T) {
	mk := func(s string) []memory.TurnRow {
		return []memory.TurnRow{{ID: uuidFor(1), Seq: 1, Status: "COMPLETED", UserText: ptr(s)}}
	}
	_, _, n := memory.BuildTranscript(mk("あいうえ"), 1000)
	require.Equal(t, 0, n)
	_, _, n = memory.BuildTranscript(mk("あいうえお"), 1000)
	require.Equal(t, 1, n)
	_, _, n = memory.BuildTranscript(mk("  あいうえ  "), 1000)
	require.Equal(t, 0, n, "trimmed")
	_, _, n = memory.BuildTranscript([]memory.TurnRow{{Seq: 1, Status: "COMPLETED"}}, 1000)
	require.Equal(t, 0, n, "nil user text")
}

func TestBuildTranscript_OrdersBySeq(t *testing.T) {
	rows := []memory.TurnRow{
		{ID: uuidFor(2), Seq: 2, Status: "COMPLETED", UserText: ptr("二番目の発話です")},
		{ID: uuidFor(1), Seq: 1, Status: "COMPLETED", UserText: ptr("一番目の発話です")},
	}
	text, _, _ := memory.BuildTranscript(rows, 1000)
	require.Equal(t, "[seq=1] USER: 一番目の発話です\n[seq=2] USER: 二番目の発話です", text)
}

func TestBuildTranscript_DropsOldestTurnsOverLimit(t *testing.T) {
	var rows []memory.TurnRow
	for i := 1; i <= 10; i++ {
		rows = append(rows, memory.TurnRow{ID: uuidFor(i), Seq: i, Status: "COMPLETED", UserText: ptr(strings.Repeat("あ", 20))})
	}
	full, valid, _ := memory.BuildTranscript(rows, 100000)
	require.Len(t, valid, 10)
	lineRunes := utf8.RuneCountInString("[seq=1] USER: " + strings.Repeat("あ", 20))
	require.Equal(t, 10*lineRunes+1+9, utf8.RuneCountInString(full), "seq 10 has a longer prefix by one rune; 9 newlines")

	// A limit exactly the size of the text keeps everything; one rune less drops exactly one turn.
	exact := utf8.RuneCountInString(full)
	same, valid, userSeqs := memory.BuildTranscript(rows, exact)
	require.Equal(t, full, same)
	require.Len(t, valid, 10)
	require.Equal(t, 10, userSeqs)

	cut, valid, userSeqs := memory.BuildTranscript(rows, exact-1)
	require.NotContains(t, cut, "[seq=1] ")
	require.True(t, strings.HasPrefix(cut, "[seq=2] USER:"))
	require.LessOrEqual(t, utf8.RuneCountInString(cut), exact-1)
	require.Len(t, valid, 9)
	require.NotContains(t, valid, 1, "a dropped turn is no longer valid evidence")
	require.Contains(t, valid, 10)
	require.Equal(t, 10, userSeqs, "the skip rule counts the whole conversation")

	// The limit is the exact size of the text without the oldest turn: that turn and its newline go.
	rest, _, _ := memory.BuildTranscript(rows[1:], 100000)
	same9, valid9, _ := memory.BuildTranscript(rows, utf8.RuneCountInString(rest))
	require.Equal(t, rest, same9)
	require.Len(t, valid9, 9)

	none, valid, _ := memory.BuildTranscript(rows, 5)
	require.Empty(t, none)
	require.Empty(t, valid)
}

func TestBuildTranscript_EmptyInput(t *testing.T) {
	text, valid, n := memory.BuildTranscript(nil, 100)
	require.Empty(t, text)
	require.NotNil(t, valid)
	require.Zero(t, n)
}

func TestNewExistingMemoryLine(t *testing.T) {
	l := memory.NewExistingMemoryLine(memory.CategoryLearningPattern, "error:x", " 行1\n行2 <b>と</b> ")
	require.Equal(t, memory.ExistingMemoryLine{Category: "LEARNING_PATTERN", Key: "error:x", Content: "行1 行2 ＜b＞と＜/b＞"}, l)
}

func TestPromptFixtures(t *testing.T) {
	fx := memory.PromptFixtures()
	require.Len(t, fx, 2)
	lo := fx[0].(memory.MemoryExtractionPromptData)
	hi := fx[1].(memory.MemoryExtractionPromptData)
	require.Empty(t, lo.ExistingMemories)
	require.Len(t, strings.Split(lo.Transcript, "\n"), 5, "3 turns: one AI-only, two with both parties")
	require.Len(t, hi.ExistingMemories, 150)
	n := utf8.RuneCountInString(hi.Transcript)
	require.LessOrEqual(t, n, 40000)
	require.Greater(t, n, 39000)
	for _, d := range []memory.MemoryExtractionPromptData{lo, hi} {
		require.NotEmpty(t, d.SessionDate)
		require.NotEmpty(t, d.ScenarioTitle)
	}
	for i, l := range hi.ExistingMemories {
		require.NotEmpty(t, l.Key, fmt.Sprint(i))
		require.LessOrEqual(t, utf8.RuneCountInString(l.Content), 60)
	}
}
