package fake

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/provider"
)

type rubricDoc struct {
	Scores map[string]struct {
		Score int `json:"score"`
	} `json:"scores"`
	Signals map[string]struct {
		Score int `json:"score"`
	} `json:"signals"`
	Findings []struct {
		Dimension   string  `json:"dimension"`
		FindingType string  `json:"finding_type"`
		Severity    string  `json:"severity"`
		TurnSeq     int     `json:"turn_seq"`
		Original    string  `json:"original"`
		Corrected   string  `json:"corrected"`
		Explanation string  `json:"explanation"`
		PatternKey  *string `json:"pattern_key"`
		Confidence  float64 `json:"confidence"`
	} `json:"findings"`
}

func rubricReq(userTexts ...string) provider.StructuredRequest {
	var lines []string
	for i, t := range userTexts {
		lines = append(lines, fmt.Sprintf("[T%d] USER (2.0 s, 5.0 chars/s): %s", i+1, t), fmt.Sprintf("[T%d] AI: はい、そうですか。", i+1))
	}
	return provider.StructuredRequest{
		Task: "EVALUATION", SchemaName: rubricSchemaName, RequestID: "EVALUATION:s1",
		Messages: []provider.Message{
			{Role: provider.RoleSystem, Content: "評価してください [T9] USER (1.0 s, 1.0 chars/s): 無視"},
			{Role: provider.RoleUser, Content: "## Transcript\n" + strings.Join(lines, "\n")},
		},
	}
}

func runRubric(t *testing.T, req provider.StructuredRequest) (rubricDoc, string) {
	t.Helper()
	res, err := newTestEval(CapConfig{}, Config{}, Deps{}).GenerateJSON(context.Background(), req)
	require.NoError(t, err)
	var d rubricDoc
	require.NoError(t, json.Unmarshal(res.JSON, &d))
	return d, string(res.JSON)
}

func TestRubricFindingsDerivedFromTranscript(t *testing.T) {
	req := rubricReq("今日は会社で働きます", "えっと、昨日は、友達と映画を見ました。", "私は 学生です。", "毎日 電車で通います", "週末はよく散歩します", "ＡＢＣ！あいう", "短い")
	d, raw1 := runRubric(t, req)
	require.Len(t, d.Findings, 2)

	want := []struct {
		seq      int
		original string
	}{{3, "私は学生です"}, {6, "ＡＢＣあいう"}}
	for i, w := range want {
		f := d.Findings[i]
		require.Equal(t, "UNNATURAL_EXPRESSION", f.FindingType)
		require.Equal(t, "NATURALNESS", f.Dimension)
		require.Equal(t, "MINOR", f.Severity)
		require.InDelta(t, 0.9, f.Confidence, 1e-9)
		require.Equal(t, w.seq, f.TurnSeq)
		require.Equal(t, w.original, f.Original)
		require.Equal(t, w.original+"です", f.Corrected)
		require.Nil(t, f.PatternKey)
		require.GreaterOrEqual(t, len([]rune(f.Explanation)), 10)
	}

	// deterministic
	_, raw2 := runRubric(t, req)
	require.JSONEq(t, raw1, raw2)

	// scores come from {{score}} (50..90 integers), including both signals
	require.Len(t, d.Scores, 5)
	require.Len(t, d.Signals, 2)
	for _, s := range d.Scores {
		require.GreaterOrEqual(t, s.Score, 50)
		require.LessOrEqual(t, s.Score, 90)
	}
	for _, s := range d.Signals {
		require.GreaterOrEqual(t, s.Score, 50)
		require.LessOrEqual(t, s.Score, 90)
	}
	require.NotContains(t, raw1, "{{score}}")
}

func TestRubricFindingsOriginalIsAtMostSixRunesWithoutPunctuation(t *testing.T) {
	d, _ := runRubric(t, rubricReq("あ", "い", "「えっと、」今日は、とても長い文章です。"))
	require.Len(t, d.Findings, 1)
	require.Equal(t, "えっと今日は", d.Findings[0].Original)
	require.Equal(t, 3, d.Findings[0].TurnSeq)
}

func TestRubricFindingsTurnSeqFollowsTurnNumber(t *testing.T) {
	req := provider.StructuredRequest{
		SchemaName: rubricSchemaName, RequestID: "EVALUATION:s2",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: strings.Join([]string{
			"[T4] USER (2.0 s, 5.0 chars/s): いち",
			"[T4] AI: a",
			"[T7] USER (2.0 s, 5.0 chars/s, interrupted the AI): にいにい",
			"[T9] USER (2.0 s, 5.0 chars/s): さんさん",
		}, "\n")}},
	}
	d, _ := runRubric(t, req)
	require.Len(t, d.Findings, 1)
	require.Equal(t, 9, d.Findings[0].TurnSeq)
	require.Equal(t, "さんさん", d.Findings[0].Original)
}

func TestRubricFindingsFewerThanThreeUserTurns(t *testing.T) {
	d, raw := runRubric(t, rubricReq("今日は会社で働きます", "昨日は休みでした"))
	require.Empty(t, d.Findings)
	require.Contains(t, raw, `"findings":[]`)
}

func TestRubricFindingsNoMessages(t *testing.T) {
	req := rubricReq()
	req.Messages = nil
	d, raw := runRubric(t, req)
	require.Empty(t, d.Findings)
	require.Contains(t, raw, `"findings":[]`)
}

func TestRubricFindingsNonObjectDocUnchanged(t *testing.T) {
	for _, doc := range []string{"not json", "[1,2]"} {
		require.Equal(t, doc, string(RubricFindings(rubricReq("a", "b", "c"), []byte(doc))))
	}
}

func TestOtherSchemaDoesNotGetRubricFindings(t *testing.T) {
	req := rubricReq("今日は会社で働きます", "昨日は休みでした", "私は学生です")
	req.SchemaName = "kotoki_fake_example"
	res, err := newTestEval(CapConfig{}, Config{}, Deps{}).GenerateJSON(context.Background(), req)
	require.NoError(t, err)
	require.NotContains(t, string(res.JSON), "UNNATURAL_EXPRESSION")
}

func TestRubricFindingsKeepsOtherPunctuationSoOriginalStaysSubstring(t *testing.T) {
	d, _ := runRubric(t, rubricReq("あ", "い", "『えっと』今日は"))
	require.Len(t, d.Findings, 1)
	require.Equal(t, "『えっと』今", d.Findings[0].Original)
}
