package bench

import (
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/nabePi/kotoki-backend/internal/provider"
)

var humanColumns = []string{"item_id", "reviewer", "candidate", "score_naturalness", "score_level", "score_character", "mos", "kanji_misread"}

// LoadHuman reads the rating sheet (spec 8.10.3, plan Task 21: CSV with the columns
// item_id,reviewer,candidate,score_naturalness,score_level,score_character,mos,kanji_misread) and
// returns one SuiteHuman result per candidate that has ratings. candidate is Candidate.Key() of a
// candidate in cands (an unknown key is an error: a typo must not silently drop paid ratings).
// LLM: quality_mean = mean of every given naturalness/level/character score (1-5). TTS: mos = mean of
// the given mos (1-5), kanji_err_rate = mean of kanji_misread (0|1). Blank cells are skipped.
func LoadHuman(path string, cands []Candidate) ([]Result, error) {
	f, err := os.Open(path) //nolint:gosec // operator-chosen file (--human)
	if err != nil {
		return nil, fmt.Errorf("bench: human ratings: %w", err)
	}
	defer func() { _ = f.Close() }()
	rd := csv.NewReader(f)
	rd.FieldsPerRecord = -1
	head, err := rd.Read()
	if err != nil {
		return nil, fmt.Errorf("bench: human ratings: header: %w", err)
	}
	col := map[string]int{}
	for i, h := range head {
		col[strings.TrimSpace(h)] = i
	}
	for _, h := range humanColumns {
		if _, ok := col[h]; !ok {
			return nil, fmt.Errorf("bench: human ratings: column %q is missing (want %s)", h, strings.Join(humanColumns, ","))
		}
	}
	type acc struct{ q, mos, kanji []float64 }
	byKey := map[string]*acc{}
	byCand := map[string][]Candidate{}
	for _, c := range cands {
		byCand[c.Key()] = append(byCand[c.Key()], c)
	}
	for line := 2; ; line++ {
		rec, err := rd.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("bench: human ratings line %d: %w", line, err)
		}
		cell := func(name string) string {
			if i := col[name]; i < len(rec) {
				return strings.TrimSpace(rec[i])
			}
			return ""
		}
		key := cell("candidate")
		cs, ok := byCand[key]
		if !ok {
			return nil, fmt.Errorf("bench: human ratings line %d: candidate %q is not in the models file", line, key)
		}
		if cs[0].Capability == provider.CapSTT {
			return nil, fmt.Errorf("bench: human ratings line %d: %s has no human ratings (STT is measured by CER)", line, key)
		}
		a := byKey[key]
		if a == nil {
			a = &acc{}
			byKey[key] = a
		}
		num := func(name string, lo, hi float64) (float64, bool, error) {
			s := cell(name)
			if s == "" {
				return 0, false, nil
			}
			v, err := strconv.ParseFloat(s, 64)
			if err != nil || v < lo || v > hi {
				return 0, false, fmt.Errorf("bench: human ratings line %d: %s %q is not a number in %g..%g", line, name, s, lo, hi)
			}
			return v, true, nil
		}
		for _, n := range []string{"score_naturalness", "score_level", "score_character"} {
			v, ok, err := num(n, 1, 5)
			if err != nil {
				return nil, err
			}
			if ok {
				a.q = append(a.q, v)
			}
		}
		if v, ok, err := num("mos", 1, 5); err != nil {
			return nil, err
		} else if ok {
			a.mos = append(a.mos, v)
		}
		if v, ok, err := num("kanji_misread", 0, 1); err != nil {
			return nil, err
		} else if ok {
			a.kanji = append(a.kanji, v)
		}
	}
	var out []Result
	for _, c := range cands {
		a := byKey[c.Key()]
		if a == nil {
			continue
		}
		r := newResult(SuiteHuman, c, Item{}, 1)
		switch c.Capability {
		case provider.CapLLM:
			if len(a.q) > 0 {
				r.Values["quality_mean"] = mean(a.q)
			}
		case provider.CapTTS:
			if len(a.mos) > 0 {
				r.Values["mos"] = mean(a.mos)
			}
			if len(a.kanji) > 0 {
				r.Values["kanji_err_rate"] = mean(a.kanji)
			}
		}
		if len(r.Values) > 0 {
			out = append(out, r)
		}
	}
	return out, nil
}

func mean(xs []float64) float64 {
	var s float64
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}
