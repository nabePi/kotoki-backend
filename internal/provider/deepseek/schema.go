package deepseek

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// schemaInstruction is the extra system message of an Evaluation request. DeepSeek JSON mode
// only guarantees syntactically valid JSON (no json_schema/strict, spec 8.4.9) and requires the
// word "json" plus an output example in the prompt, so the message carries the instruction, the
// schema text and an empty skeleton generated from the schema.
func schemaInstruction(schema json.RawMessage, schemaName string) (string, error) {
	sk, err := skeleton(schema)
	if err != nil {
		return "", err
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, schema); err != nil {
		return "", fmt.Errorf("deepseek: schema is not valid JSON: %w", err)
	}
	return "Output JSON only: a single JSON object, with no prose, no code fences and no text before or after it.\n" +
		"The JSON object MUST conform to this JSON Schema (" + schemaName + "):\n" + compact.String() + "\n" +
		"EXAMPLE JSON OUTPUT (structure only, replace every value with real content):\n" + string(sk), nil
}

// skeleton builds an empty example of the schema: every required property of an object gets a
// zero value for its type (integer/number 0, string "", boolean false, array [], nullable null),
// nested objects recursively, enum its first element.
func skeleton(schema json.RawMessage) (json.RawMessage, error) {
	var n any
	dec := json.NewDecoder(bytes.NewReader(schema))
	dec.UseNumber()
	if err := dec.Decode(&n); err != nil {
		return nil, fmt.Errorf("deepseek: schema is not valid JSON: %w", err)
	}
	if _, ok := n.(map[string]any); !ok {
		return nil, errors.New("deepseek: schema must be a JSON object")
	}
	return json.Marshal(zeroValue(n, 0))
}

const maxSkeletonDepth = 16

func zeroValue(n any, depth int) any {
	m, ok := n.(map[string]any)
	if !ok || depth > maxSkeletonDepth {
		return nil
	}
	if c, ok := m["const"]; ok {
		return c
	}
	if e, ok := m["enum"].([]any); ok && len(e) > 0 {
		return e[0]
	}
	for _, key := range []string{"anyOf", "oneOf"} {
		if alts, ok := m[key].([]any); ok && len(alts) > 0 {
			return zeroValue(alts[0], depth+1)
		}
	}
	switch t := schemaType(m); t {
	case "object":
		out := map[string]any{}
		props, _ := m["properties"].(map[string]any)
		req, _ := m["required"].([]any)
		for _, r := range req {
			name, ok := r.(string)
			if !ok {
				continue
			}
			out[name] = zeroValue(props[name], depth+1)
		}
		return out
	case "array":
		return []any{}
	case "string":
		return ""
	case "integer", "number":
		return 0
	case "boolean":
		return false
	default:
		return nil
	}
}

// schemaType returns the effective type: "null" when the type list admits null (nullable
// property), otherwise the first listed type; an untyped schema with properties is an object.
func schemaType(m map[string]any) string {
	switch t := m["type"].(type) {
	case string:
		return t
	case []any:
		first := ""
		for _, v := range t {
			s, _ := v.(string)
			if s == "null" {
				return "null"
			}
			if first == "" {
				first = s
			}
		}
		return first
	}
	if _, ok := m["properties"]; ok {
		return "object"
	}
	return ""
}
