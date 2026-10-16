package memory_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/memory"
)

func seedSchema(t *testing.T) json.RawMessage {
	t.Helper()
	tpl, _ := seedV1(t)
	return tpl.OutputSchema
}

const oneMemory = `{"category":"PROFILE_FACT","key":"occupation","content_ja":"エンジニア。","display_text_id":"Insinyur.",
 "evidence_type":"EXPLICIT_USER_STATEMENT","confidence":0.9,"importance":0.8,"is_temporary":false,"valid_days":null,
 "source_turn_seqs":[1],"sensitive":false}`

func wrap(mems, episode string) []byte {
	return []byte(`{"memories":[` + mems + `],"episode":` + episode + `}`)
}

func TestParseOutput_Valid(t *testing.T) {
	out, err := memory.ParseOutput(wrap(oneMemory, "null"), seedSchema(t))
	require.NoError(t, err)
	require.Len(t, out.Memories, 1)
	require.Nil(t, out.Episode)
	m := out.Memories[0]
	require.Equal(t, "PROFILE_FACT", m.Category)
	require.Equal(t, "occupation", m.Key)
	require.Equal(t, "エンジニア。", m.ContentJA)
	require.InDelta(t, 0.9, m.Confidence, 1e-9)
	require.Nil(t, m.ValidDays)
	require.Equal(t, []int{1}, m.SourceTurnSeqs)

	out, err = memory.ParseOutput([]byte(`{"memories":[],"episode":{"content_ja":"話した。","display_text_id":"Bicara.","confidence":0.7,"importance":0.5,"source_turn_seqs":[2,3]}}`), seedSchema(t))
	require.NoError(t, err)
	require.Empty(t, out.Memories)
	require.NotNil(t, out.Episode)
	require.Equal(t, []int{2, 3}, out.Episode.SourceTurnSeqs)

	temp := strings.ReplaceAll(strings.Replace(oneMemory, `"is_temporary":false,"valid_days":null`, `"is_temporary":true,"valid_days":45`, 1), "\n", "")
	out, err = memory.ParseOutput(wrap(temp, "null"), seedSchema(t))
	require.NoError(t, err)
	require.Equal(t, 45, *out.Memories[0].ValidDays)
}

func TestParseOutput_Invalid(t *testing.T) {
	long := func(n int) string { return strings.Repeat("あ", n) }
	rep := func(from, to string) []byte { return wrap(strings.Replace(oneMemory, from, to, 1), "null") }
	cases := map[string][]byte{
		"not json":                 []byte("memories: none"),
		"empty":                    nil,
		"array root":               []byte(`[]`),
		"missing episode":          []byte(`{"memories":[]}`),
		"missing memories":         []byte(`{"episode":null}`),
		"extra root field":         []byte(`{"memories":[],"episode":null,"x":1}`),
		"extra item field":         rep(`"sensitive":false`, `"sensitive":false,"note":"x"`),
		"missing item field":       rep(`"importance":0.8,`, ``),
		"bad category":             rep(`"PROFILE_FACT"`, `"EPISODIC"`),
		"bad evidence":             rep(`"EXPLICIT_USER_STATEMENT"`, `"GUESS"`),
		"content over 60":          rep(`"エンジニア。"`, `"`+long(61)+`"`),
		"display over 160":         rep(`"Insinyur."`, `"`+strings.Repeat("a", 161)+`"`),
		"key over 100":             rep(`"occupation"`, `"`+strings.Repeat("a", 101)+`"`),
		"confidence over 1":        rep(`"confidence":0.9`, `"confidence":1.5`),
		"importance below 0":       rep(`"importance":0.8`, `"importance":-0.1`),
		"valid_days 121":           rep(`"valid_days":null`, `"valid_days":121`),
		"valid_days fractional":    rep(`"valid_days":null`, `"valid_days":3.5`),
		"valid_days string":        rep(`"valid_days":null`, `"valid_days":"7"`),
		"sensitive string":         rep(`"sensitive":false`, `"sensitive":"no"`),
		"no source seqs":           rep(`"source_turn_seqs":[1]`, `"source_turn_seqs":[]`),
		"11 source seqs":           rep(`"source_turn_seqs":[1]`, `"source_turn_seqs":[1,2,3,4,5,6,7,8,9,10,11]`),
		"fractional seq":           rep(`"source_turn_seqs":[1]`, `"source_turn_seqs":[1.5]`),
		"13 memories":              wrap(strings.TrimSuffix(strings.Repeat(oneMemory+",", 13), ","), "null"),
		"episode content over 150": wrap("", `{"content_ja":"`+long(151)+`","display_text_id":"x","confidence":0.7,"importance":0.5,"source_turn_seqs":[1]}`),
		"episode missing field":    wrap("", `{"content_ja":"話した。","display_text_id":"x","confidence":0.7,"source_turn_seqs":[1]}`),
		"episode wrong type":       wrap("", `"episode"`),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := memory.ParseOutput(raw, seedSchema(t))
			require.ErrorIs(t, err, memory.ErrInvalidOutput)
		})
	}
}

func TestParseOutput_ErrorNamesLocationNotContent(t *testing.T) {
	raw := wrap(strings.Replace(oneMemory, `"エンジニア。"`, `"`+strings.Repeat("秘", 61)+`"`, 1), "null")
	_, err := memory.ParseOutput(raw, seedSchema(t))
	require.ErrorIs(t, err, memory.ErrInvalidOutput)
	require.NotContains(t, err.Error(), "秘")
	require.Contains(t, err.Error(), "/memories/0/content_ja")
}

func TestParseOutput_StrictDecodeAndTrailingData(t *testing.T) {
	// A schema that does not forbid extra properties still cannot smuggle a field past the decoder.
	loose := json.RawMessage(`{"type":"object"}`)
	_, err := memory.ParseOutput([]byte(`{"memories":[],"episode":null,"extra":1}`), loose)
	require.ErrorIs(t, err, memory.ErrInvalidOutput)
	_, err = memory.ParseOutput([]byte(`{"memories":"x","episode":null}`), loose)
	require.ErrorIs(t, err, memory.ErrInvalidOutput)
	_, err = memory.ParseOutput([]byte(`{"memories":[],"episode":null}`), loose)
	require.NoError(t, err)
	_, err = memory.ParseOutput([]byte(`{"memories":[],"episode":null} {"memories":[]}`), loose)
	require.ErrorIs(t, err, memory.ErrInvalidOutput)
	_, err = memory.ParseOutput([]byte(`{"memories":[],"episode":null} x`), loose)
	require.ErrorIs(t, err, memory.ErrInvalidOutput)
}

func TestParseOutput_BrokenSchemaIsNotModelError(t *testing.T) {
	for _, s := range []string{"", "   ", "not json", `{"type":"nonsense"}`} {
		_, err := memory.ParseOutput([]byte(`{"memories":[],"episode":null}`), json.RawMessage(s))
		require.Error(t, err, s)
		require.NotErrorIs(t, err, memory.ErrInvalidOutput, s)
	}
}

func TestParseOutput_SchemaDraft2020(t *testing.T) {
	// prefixItems exists only in draft 2020-12; under an older draft it would be ignored.
	schema := json.RawMessage(`{"type":"object","properties":{"memories":{"type":"array","prefixItems":[{"type":"string"}]}}}`)
	_, err := memory.ParseOutput([]byte(`{"memories":[1],"episode":null}`), schema)
	require.ErrorIs(t, err, memory.ErrInvalidOutput)
	_, err = memory.ParseOutput([]byte(`{"memories":[],"episode":null}`), schema)
	require.NoError(t, err)
}

func TestParseOutput_ErrorListsAtMostFiveLocations(t *testing.T) {
	var items []string
	for i := 0; i < 8; i++ {
		items = append(items, strings.Replace(oneMemory, `"confidence":0.9`, `"confidence":2`, 1))
	}
	_, err := memory.ParseOutput(wrap(strings.Join(items, ","), "null"), seedSchema(t))
	require.ErrorIs(t, err, memory.ErrInvalidOutput)
	require.Equal(t, 5, strings.Count(err.Error(), "/memories/"), err.Error())
}
