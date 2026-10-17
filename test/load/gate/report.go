package gate

import (
	"fmt"
	"sort"
	"strings"
)

// RenderMarkdown renders the per-criterion table of a gate run for docs/loadtest/<date>-<gate>.md.
// meta carries free-form header lines (date, commit, environment, ...) rendered in key order.
func RenderMarkdown(s Spec, rs []Result, meta map[string]string) string {
	var b strings.Builder
	verdict := "GAGAL"
	if AllPass(rs) {
		verdict = "LULUS"
	}
	fmt.Fprintf(&b, "# Load test %s: %s\n\n", s.Name, verdict)
	if d := meta["date"]; d != "" {
		fmt.Fprintf(&b, "- Tanggal: %s\n", cell(d))
	}
	keys := make([]string, 0, len(meta))
	for k := range meta {
		if k != "date" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "- %s: %s\n", cell(k), cell(meta[k]))
	}
	fmt.Fprintf(&b, "- Gate: %s, %d VU, ramp %s, hold %s, mode %s\n\n", s.Name, s.VUs, s.Ramp, s.Hold, s.Mode)
	b.WriteString("| # | Kriteria | Ambang | Nilai | Hasil | Catatan |\n|---|---|---|---|---|---|\n")
	for _, r := range rs {
		status, value := "FAIL", fmt.Sprintf("%g", r.Value)
		switch {
		case r.Skipped:
			status, value = "SKIP", "-"
		case r.Pass:
			status = "PASS"
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s |\n", r.ID, cell(r.Name), cell(r.Threshold), value, status, cell(r.Reason))
	}
	return b.String()
}

func cell(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.ReplaceAll(s, "|", `\|`)
}
