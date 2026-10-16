package memory_test

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/memory"
	"github.com/nabePi/kotoki-backend/internal/prompt"
)

const (
	seedFile     = "../../seed/0070_prompts.sql"
	specFile     = "../../docs/superpowers/specs/2026-10-12-kotoki-backend-design.md"
	memoryPVID   = "01920000-000d-7000-8000-000000000005"
	memoryCfgID  = "01920000-000c-7000-8000-000000000005"
	tplDelimiter = "$tpl$"
)

// seedV1 reads the MEMORY_EXTRACTION version 1 tuple out of the seed file: the two dollar-quoted
// templates, the output_schema literal and the llm_params literal.
func seedV1(t *testing.T) (tpl prompt.Template, llmParams string) {
	t.Helper()
	b, err := os.ReadFile(seedFile)
	require.NoError(t, err)
	s := string(b)
	i := strings.Index(s, "('"+memoryPVID+"', '"+memoryCfgID+"', 1, 'PUBLISHED',")
	require.GreaterOrEqual(t, i, 0, "MEMORY_EXTRACTION v1 tuple missing from the seed")
	rest := s[i:]
	var parts []string
	for range 2 {
		a := strings.Index(rest, tplDelimiter)
		require.GreaterOrEqual(t, a, 0)
		rest = rest[a+len(tplDelimiter):]
		e := strings.Index(rest, tplDelimiter)
		require.GreaterOrEqual(t, e, 0)
		parts = append(parts, rest[:e])
		rest = rest[e+len(tplDelimiter):]
	}
	m := regexp.MustCompile(`(?s)^,\s*'(\{.*?\})',\s*'(\{.*?\})'::jsonb`).FindStringSubmatch(rest)
	require.NotNil(t, m, "output_schema and llm_params literals not found after the templates")
	return prompt.Template{Key: "MEMORY_EXTRACTION", Version: 1, System: parts[0], User: parts[1], OutputSchema: json.RawMessage(m[1])}, m[2]
}

// specBlock returns the fenced block that follows marker inside spec 11.5.2.
func specBlock(t *testing.T, marker string) string {
	t.Helper()
	b, err := os.ReadFile(specFile)
	require.NoError(t, err)
	s := string(b)
	from := strings.Index(s, "#### 11.5.2")
	to := strings.Index(s, "#### 11.5.3")
	require.True(t, from >= 0 && to > from)
	sec := s[from:to]
	k := strings.Index(sec, marker)
	require.GreaterOrEqual(t, k, 0, marker)
	m := regexp.MustCompile("(?s)```\\w*\n(.*?)\n```").FindStringSubmatch(sec[k:])
	require.NotNil(t, m)
	return m[1]
}

func TestSeedV1IsVerbatimSpec(t *testing.T) {
	tpl, params := seedV1(t)
	require.Equal(t, specBlock(t, "`system_template` (seed v1):"), tpl.System)
	require.Equal(t, specBlock(t, "`user_template` (seed v1):"), tpl.User)
	var seedSchema, specSchema any
	require.NoError(t, json.Unmarshal(tpl.OutputSchema, &seedSchema))
	require.NoError(t, json.Unmarshal([]byte(specBlock(t, "`output_schema` seed v1")), &specSchema))
	require.Equal(t, specSchema, seedSchema)
	require.JSONEq(t, `{"temperature":0,"max_output_tokens":2000}`, params)
	require.JSONEq(t, specBlock(t, "Contoh output:"), string(memory.ExampleOutput()))
}

func TestRenderSeedV1WithFixtures(t *testing.T) {
	tpl, _ := seedV1(t)
	fx := memory.PromptFixtures()
	require.Len(t, fx, 2)
	for i, f := range fx {
		data := f.(memory.MemoryExtractionPromptData)
		out, err := prompt.Render(tpl, data)
		require.NoError(t, err, "fixture %d", i)
		require.NotContains(t, out.System, "{{")
		require.Contains(t, out.System, "ALLOWED KEYS")
		require.Contains(t, out.User, "SESSION DATE: "+data.SessionDate+"\n")
		require.Contains(t, out.User, "SCENARIO: "+data.ScenarioTitle+"\n")
		require.Contains(t, out.User, "<transcript>\n"+data.Transcript+"\n</transcript>")
		require.True(t, strings.HasSuffix(out.User, "</transcript>"))
		if len(data.ExistingMemories) == 0 {
			require.Contains(t, out.User, "EXISTING MEMORIES (category | key | content):\n(none)\n\n<transcript>")
		} else {
			require.NotContains(t, out.User, "(none)")
			for _, l := range data.ExistingMemories {
				require.Contains(t, out.User, l.Category+" | "+l.Key+" | "+l.Content+"\n")
			}
		}
	}
	// A misspelled placeholder fails instead of rendering empty (missingkey=error).
	bad := tpl
	bad.User = strings.Replace(bad.User, "{{.Transcript}}", "{{.transcript}}", 1)
	_, err := prompt.Render(bad, fx[0])
	require.Error(t, err)

	// Existing memories render one per line between the header and the transcript.
	data := fx[0].(memory.MemoryExtractionPromptData)
	data.ExistingMemories = []memory.ExistingMemoryLine{
		memory.NewExistingMemoryLine(memory.CategoryProfileFact, "occupation", "エンジニア。"),
		memory.NewExistingMemoryLine(memory.CategoryPreference, "pref:formality", "丁寧語を好む。"),
	}
	out, err := prompt.Render(tpl, data)
	require.NoError(t, err)
	require.Contains(t, out.User, "EXISTING MEMORIES (category | key | content):\nPROFILE_FACT | occupation | エンジニア。\nPREFERENCE | pref:formality | 丁寧語を好む。\n\n<transcript>")
}

func TestExampleOutputPassesSeedSchema(t *testing.T) {
	tpl, _ := seedV1(t)
	out, err := memory.ParseOutput(memory.ExampleOutput(), tpl.OutputSchema)
	require.NoError(t, err)
	require.Len(t, out.Memories, 3)
	require.NotNil(t, out.Episode)
}

func TestFakeFixtureValidates(t *testing.T) {
	tpl, _ := seedV1(t)
	raw, err := os.ReadFile("../provider/fake/fixtures/kotoki_memory_extraction_v1.json")
	require.NoError(t, err)
	out, err := memory.ParseOutput(raw, tpl.OutputSchema)
	require.NoError(t, err)

	rows := []memory.TurnRow{
		{Seq: 0, Status: "COMPLETED", AssistantText: "こんにちは。"},
		{Seq: 1, Status: "COMPLETED", UserText: ptr("バックエンドエンジニアです。"), AssistantText: "そうですか。"},
		{Seq: 2, Status: "COMPLETED", UserText: ptr("ゆっくり話してください。"), AssistantText: "はい。"},
		{Seq: 3, Status: "COMPLETED", UserText: ptr("私は「は」と「が」を間違えます。"), AssistantText: "なるほど。"},
		{Seq: 4, Status: "COMPLETED", UserText: ptr("ありがとうございました。"), AssistantText: "また。"},
	}
	for i := range rows {
		rows[i].ID = uuidFor(i)
	}
	_, turns, _ := memory.BuildTranscript(rows, memory.TranscriptMaxRunes)
	m, reg := newMetrics(t)
	cands := memory.ValidateCandidates(out, turns, uuidFor(99), m)
	require.Len(t, cands, 4, "3 memories and the episode survive the 12 rules")
	require.Equal(t, memory.CategoryEpisodic, cands[3].Category)
	for _, seqs := range [][]int{out.Memories[0].SourceTurnSeqs, out.Memories[1].SourceTurnSeqs, out.Memories[2].SourceTurnSeqs, out.Episode.SourceTurnSeqs} {
		for _, s := range seqs {
			require.Contains(t, turns, s)
			require.LessOrEqual(t, s, 3, "fixture evidence stays inside seq 1-3")
		}
	}
	require.Equal(t, []int{1}, out.Memories[0].SourceTurnSeqs)
	require.Equal(t, []int{2}, out.Memories[1].SourceTurnSeqs)
	require.Equal(t, []int{1, 2, 3}, out.Memories[2].SourceTurnSeqs)
	require.Equal(t, []int{1, 2, 3}, out.Episode.SourceTurnSeqs)
	require.Empty(t, rejectedCounts(t, reg))
}
