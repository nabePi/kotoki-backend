package evaluation

import (
	"cmp"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/google/uuid"
	"golang.org/x/text/unicode/norm"
)

// Drop reasons (label values of kotoki_evaluation_findings_dropped_total). The first six are
// rejections by the validity rules and count toward FilterResult.Rejected.
const (
	DropTypeDimensionMismatch = "type_dimension_mismatch"
	DropMissingField          = "missing_field"
	DropMustBeNull            = "must_be_null"
	DropTurnNotFound          = "turn_not_found"
	DropOriginalNotFound      = "original_not_found"
	DropCorrectedUnchanged    = "corrected_unchanged"
	DropLowConfidence         = "low_confidence"
	DropDuplicate             = "duplicate"
	DropOverLimit             = "over_limit"
)

const (
	maxErrorFindings    = 15
	maxStrengthFindings = 3
	sourceLLM           = "LLM"
	ignoredPunctuation  = "、。，．,.！？!?「」…・"
)

// TurnRef is a USER turn of the transcript a finding may point at.
type TurnRef struct {
	ID       uuid.UUID
	Seq      int
	UserText string
}

// Finding is a validated finding ready to be stored.
type Finding struct {
	Type                FindingType
	Dimension           Dimension
	Severity            Severity
	TurnSeq             *int
	TurnID              *uuid.UUID
	Original, Corrected *string
	PatternKey          *string
	Explanation         string
	Confidence          float64
	Source              string // "LLM"
}

// FilterResult is the outcome of FilterFindings. Rejected counts only findings dropped by the
// validity rules (not low confidence, duplicates or the count limits); Total is the raw count.
type FilterResult struct {
	Kept            []Finding
	Dropped         map[string]int
	Rejected, Total int
}

// TooManyRejected reports whether more than half of the findings were rejected, which makes the
// whole output invalid (repair).
func (r FilterResult) TooManyRejected() bool { return r.Total > 0 && r.Rejected*2 > r.Total }

// NormalizeForMatch is the comparison form of a text: NFKC, without whitespace and without
// 、。，．,.！？!?「」…・.
func NormalizeForMatch(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || strings.ContainsRune(ignoredPunctuation, r) {
			return -1
		}
		return r
	}, norm.NFKC.String(s))
}

func validDimension(d Dimension) bool { return slices.Contains(LLMDimensions, d) }

func blank(p *string) bool { return p == nil || strings.TrimSpace(*p) == "" }

func checkReq(r Req, missing, present bool) string {
	switch r {
	case ReqRequired:
		if missing {
			return DropMissingField
		}
	case ReqNull:
		if present {
			return DropMustBeNull
		}
	case ReqOptional:
	}
	return ""
}

// validate applies the validity rules; "" means the finding is valid.
func validate(f *Finding, byTurn map[int]TurnRef) (reason string, turn TurnRef) {
	spec, ok := SpecOf(f.Type)
	if !ok {
		return DropTypeDimensionMismatch, turn
	}
	if f.Type == FindingStrength {
		if !validDimension(f.Dimension) {
			return DropTypeDimensionMismatch, turn
		}
		f.Severity, f.Corrected = SeverityInfo, nil
	} else if f.Dimension != spec.Dimension {
		return DropTypeDimensionMismatch, turn
	}
	if r := checkReq(spec.TurnSeq, f.TurnSeq == nil, f.TurnSeq != nil); r != "" {
		return r, turn
	}
	if r := checkReq(spec.Original, blank(f.Original), f.Original != nil); r != "" {
		return r, turn
	}
	if r := checkReq(spec.Corrected, blank(f.Corrected), f.Corrected != nil); r != "" {
		return r, turn
	}
	if f.TurnSeq != nil {
		t, ok := byTurn[*f.TurnSeq]
		if !ok {
			return DropTurnNotFound, turn
		}
		turn = t
	}
	if f.Original != nil {
		orig := NormalizeForMatch(*f.Original)
		if f.TurnSeq == nil || orig == "" || !strings.Contains(NormalizeForMatch(turn.UserText), orig) {
			return DropOriginalNotFound, turn
		}
		if spec.Corrected == ReqRequired && NormalizeForMatch(*f.Corrected) == orig {
			return DropCorrectedUnchanged, turn
		}
	}
	return "", turn
}

func severityRank(s Severity) int {
	switch s {
	case SeverityMajor:
		return 2
	case SeverityMinor:
		return 1
	default:
		return 0
	}
}

// FilterFindings applies the per-finding rules of spec 10.7.2 in this order: validity rules
// (type/dimension, required/null fields, turn, original, corrected; STRENGTH is forced to INFO with
// corrected null), minimum confidence, duplicates (first wins), then the limits of 15 non-STRENGTH
// (MAJOR first, then highest confidence) and 3 STRENGTH (highest confidence). Kept keeps the order
// of raw.
func FilterFindings(raw []RawFinding, turns []TurnRef, minConfidence float64) FilterResult {
	res := FilterResult{Dropped: map[string]int{}, Total: len(raw)}
	byTurn := make(map[int]TurnRef, len(turns))
	for _, t := range turns {
		if _, dup := byTurn[t.Seq]; !dup {
			byTurn[t.Seq] = t
		}
	}
	drop := func(reason string, rejected bool) {
		res.Dropped[reason]++
		if rejected {
			res.Rejected++
		}
	}

	type cand struct {
		f   Finding
		idx int
	}
	var cands []cand
	seen := map[string]bool{}
	for i, rf := range raw {
		f := Finding{
			Type: FindingType(rf.FindingType), Dimension: Dimension(rf.Dimension), Severity: Severity(rf.Severity),
			TurnSeq: rf.TurnSeq, Original: rf.Original, Corrected: rf.Corrected, PatternKey: rf.PatternKey,
			Explanation: rf.Explanation, Confidence: rf.Confidence, Source: sourceLLM,
		}
		reason, turn := validate(&f, byTurn)
		if reason != "" {
			drop(reason, true)
			continue
		}
		if f.Confidence < minConfidence {
			drop(DropLowConfidence, false)
			continue
		}
		if f.TurnSeq != nil {
			id := turn.ID
			f.TurnID = &id
		}
		key := string(f.Type) + "|"
		if f.TurnSeq != nil {
			key += strconv.Itoa(*f.TurnSeq)
		}
		if f.Original != nil {
			key += "|" + NormalizeForMatch(*f.Original)
		}
		if seen[key] {
			drop(DropDuplicate, false)
			continue
		}
		seen[key] = true
		cands = append(cands, cand{f, i})
	}

	keep := make(map[int]bool, len(cands))
	for _, strength := range []bool{false, true} {
		var group []cand
		for _, c := range cands {
			if (c.f.Type == FindingStrength) == strength {
				group = append(group, c)
			}
		}
		slices.SortStableFunc(group, func(a, b cand) int {
			if c := cmp.Compare(severityRank(b.f.Severity), severityRank(a.f.Severity)); c != 0 {
				return c
			}
			return cmp.Compare(b.f.Confidence, a.f.Confidence)
		})
		limit := maxErrorFindings
		if strength {
			limit = maxStrengthFindings
		}
		for j, c := range group {
			if j < limit {
				keep[c.idx] = true
			} else {
				drop(DropOverLimit, false)
			}
		}
	}
	for _, c := range cands {
		if keep[c.idx] {
			res.Kept = append(res.Kept, c.f)
		}
	}
	return res
}
