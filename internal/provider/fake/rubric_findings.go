package fake

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/nabePi/kotoki-backend/internal/provider"
)

const (
	rubricSchemaName    = "kotoki_evaluation_rubric_v1"
	rubricFindingEvery  = 3
	rubricOriginalRunes = 6
)

var userTurnLineRE = regexp.MustCompile(`(?m)^\[T(\d+)\] USER[^:\n]*: (.+)$`)

// rubricPunct are the runes (besides whitespace) the evaluation filter ignores when matching
// `original`; stripping exactly this set keeps `original` a normalized substring of user_text.
const rubricPunct = "、。，．,.！？!?「」…・"

// RubricFindings replaces "findings" of the rubric fixture doc with findings derived from the
// USER lines of the transcript in the last message (spec 10.13): one UNNATURAL_EXPRESSION finding
// for every third USER turn. doc is returned unchanged when it is not a JSON object.
func RubricFindings(req provider.StructuredRequest, doc []byte) []byte {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(doc, &obj); err != nil {
		return doc
	}
	findings := []map[string]any{}
	if n := len(req.Messages); n > 0 {
		for i, m := range userTurnLineRE.FindAllStringSubmatch(req.Messages[n-1].Content, -1) {
			if (i+1)%rubricFindingEvery != 0 {
				continue
			}
			seq, err := strconv.Atoi(m[1])
			if err != nil {
				continue
			}
			original := rubricOriginal(m[2])
			if original == "" {
				continue
			}
			findings = append(findings, map[string]any{
				"dimension": "NATURALNESS", "finding_type": "UNNATURAL_EXPRESSION", "severity": "MINOR",
				"turn_seq": seq, "original": original, "corrected": original + "です",
				"explanation": "Ungkapan ini terdengar kurang alami; coba akhiri dengan bentuk sopan です.",
				"pattern_key": nil, "confidence": 0.9,
			})
		}
	}
	b, err := json.Marshal(findings)
	if err != nil {
		return doc
	}
	obj["findings"] = b
	out, err := json.Marshal(obj)
	if err != nil {
		return doc
	}
	return out
}

// rubricOriginal is the first rubricOriginalRunes runes of text without whitespace/punctuation.
func rubricOriginal(text string) string {
	var sb strings.Builder
	n := 0
	for _, r := range text {
		if unicode.IsSpace(r) || strings.ContainsRune(rubricPunct, r) {
			continue
		}
		sb.WriteRune(r)
		if n++; n == rubricOriginalRunes {
			break
		}
	}
	return sb.String()
}
