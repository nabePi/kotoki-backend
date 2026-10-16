package deepseek_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/provider/deepseek"
)

func TestSkeletonFromSchema(t *testing.T) {
	tests := []struct{ name, schema, want string }{
		{"flat types", `{"type":"object","properties":{"i":{"type":"integer"},"n":{"type":"number"},"s":{"type":"string"},"b":{"type":"boolean"},"a":{"type":"array","items":{"type":"string"}}},"required":["i","n","s","b","a"]}`,
			`{"i":0,"n":0,"s":"","b":false,"a":[]}`},
		{"nullable", `{"type":"object","properties":{"x":{"type":["string","null"]},"y":{"type":["null","integer"]}},"required":["x","y"]}`,
			`{"x":null,"y":null}`},
		{"nested", `{"type":"object","properties":{"o":{"type":"object","properties":{"k":{"type":"integer"},"deep":{"type":"object","properties":{"z":{"type":"boolean"}},"required":["z"]}},"required":["k","deep"]}},"required":["o"]}`,
			`{"o":{"k":0,"deep":{"z":false}}}`},
		{"enum first", `{"type":"object","properties":{"e":{"type":"string","enum":["B","A"]},"n":{"enum":[3,4]}},"required":["e","n"]}`,
			`{"e":"B","n":3}`},
		{"only required", `{"type":"object","properties":{"r":{"type":"integer"},"opt":{"type":"integer"}},"required":["r"]}`,
			`{"r":0}`},
		{"anyOf first", `{"type":"object","properties":{"u":{"anyOf":[{"type":"string"},{"type":"null"}]}},"required":["u"]}`,
			`{"u":""}`},
		{"untyped with properties", `{"properties":{"q":{"type":"string"}},"required":["q"]}`, `{"q":""}`},
		{"empty object", `{"type":"object"}`, `{}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := deepseek.Skeleton(json.RawMessage(tc.schema))
			require.NoError(t, err)
			require.JSONEq(t, tc.want, string(got))
		})
	}
}

func TestSkeletonRejectsBadSchema(t *testing.T) {
	for _, s := range []string{`{`, `[]`, `"x"`, `3`} {
		_, err := deepseek.Skeleton(json.RawMessage(s))
		require.Error(t, err, s)
	}
}

func TestSkeletonDepthLimited(t *testing.T) {
	// a self-nesting schema deeper than the limit must terminate
	s := `{"type":"string"}`
	for i := 0; i < 40; i++ {
		s = `{"type":"object","properties":{"c":` + s + `},"required":["c"]}`
	}
	got, err := deepseek.Skeleton(json.RawMessage(s))
	require.NoError(t, err)
	var v any = map[string]any{}
	require.NoError(t, json.Unmarshal(got, &v))
	depth := 0
	for {
		m, ok := v.(map[string]any)
		if !ok {
			break
		}
		v = m["c"]
		depth++
	}
	require.Equal(t, 17, depth, "levels beyond the depth limit collapse to null")
	require.Nil(t, v)
}

func TestSchemaInstructionContainsJSONWord(t *testing.T) {
	schema := json.RawMessage(`{ "type":"object", "properties":{"s":{"type":"integer"}}, "required":["s"], "additionalProperties":false }`)
	got, err := deepseek.SchemaInstruction(schema, "kotoki_evaluation_rubric_v1")
	require.NoError(t, err)
	require.Contains(t, got, "JSON", "DeepSeek JSON mode requires the word json in the prompt")
	require.True(t, strings.HasPrefix(got, "Output JSON only"), "the instruction opens with the JSON-only demand")
	require.Contains(t, got, `{"type":"object","properties":{"s":{"type":"integer"}},"required":["s"],"additionalProperties":false}`, "schema text, compacted")
	require.Contains(t, got, `{"s":0}`, "empty skeleton example")
	require.Contains(t, got, "kotoki_evaluation_rubric_v1")
	require.False(t, strings.HasSuffix(got, "\n"))
}

func TestSchemaInstructionRejectsBadSchema(t *testing.T) {
	_, err := deepseek.SchemaInstruction(json.RawMessage(`[1]`), "n")
	require.Error(t, err)
	_, err = deepseek.SchemaInstruction(json.RawMessage(`{`), "n")
	require.Error(t, err)
}

func TestInjectsInstructionAfterSystemBlock(t *testing.T) {
	ms := []provider.Message{
		{Role: provider.RoleSystem, Content: "S1"}, {Role: provider.RoleSystem, Content: "S2"},
		{Role: provider.RoleUser, Content: "U"}, {Role: provider.RoleSystem, Content: "late"},
	}
	got := deepseek.WithSchemaInstruction(ms, "I")
	require.Equal(t, []string{"S1", "S2", "I", "U", "late"}, contents(got))
	require.Equal(t, provider.RoleSystem, got[2].Role)
	require.Equal(t, []string{"S1", "S2", "U", "late"}, contents(ms), "input is not modified")

	got = deepseek.WithSchemaInstruction([]provider.Message{{Role: provider.RoleSystem, Content: "S"}}, "I")
	require.Equal(t, []string{"S", "I"}, contents(got), "system-only input keeps the instruction last")

	got = deepseek.WithSchemaInstruction([]provider.Message{{Role: provider.RoleUser, Content: "U"}}, "I")
	require.Equal(t, []string{"I", "U"}, contents(got))
}

func contents(ms []provider.Message) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.Content
	}
	return out
}
