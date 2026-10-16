package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/nabePi/kotoki-backend/cmd/kotoki-cli/internal/report"
	"github.com/nabePi/kotoki-backend/internal/provider/pricing"
)

// reportFlags are the flags of `kotoki-cli provider-report` (spec 8.8.3).
type reportFlags struct {
	from, to, format, prices string
}

// cmdProviderReport is the offline report per provider stack: latency, failure rate, TTS fallback
// rate, cost and evaluation score from session_turns. It reads Postgres through a read-only
// connection (DATABASE_URL, DB_URL as a fallback); the range is [from, to).
func (a *app) cmdProviderReport(ctx context.Context, args []string) error {
	var f reportFlags
	fs := a.newFlagSet("provider-report")
	fs.StringVar(&f.from, "from", "", "start of the range, inclusive: YYYY-MM-DD (UTC) or RFC 3339 (required)")
	fs.StringVar(&f.to, "to", "", "end of the range, exclusive: YYYY-MM-DD (UTC) or RFC 3339 (required)")
	fs.StringVar(&f.format, "format", "csv", "csv|json")
	fs.StringVar(&f.prices, "prices", "", "price table file (default: the embedded prices.yaml)")
	if err := a.parse(fs, args); err != nil {
		return err
	}
	from, to, err := f.validate()
	if err != nil {
		return err
	}
	url := a.getenv("DATABASE_URL")
	if url == "" {
		url = a.getenv("DB_URL")
	}
	if url == "" {
		return usagef("DATABASE_URL (or DB_URL) is required: the report reads session_turns")
	}
	prices, err := pricing.Load(f.prices)
	if err != nil {
		return fmt.Errorf("prices: %w", err)
	}
	pool, err := report.OpenReadOnly(ctx, url)
	if err != nil {
		return err
	}
	defer pool.Close()
	aggs, err := report.Query(ctx, pool, from, to)
	if err != nil {
		return err
	}
	rows := report.Build(aggs, prices)
	for _, r := range rows {
		if len(r.Unpriced) > 0 {
			_, _ = fmt.Fprintf(a.errw, "kotoki-cli: provider-report: no price for %s of stack stt=%s llm=%s/%s tts=%s/%s (counted as 0; fill prices.yaml, STT needs a model \"*\" entry because session_turns stores no STT model)\n",
				strings.Join(r.Unpriced, "+"), r.SttProvider, r.LLMProvider, r.LLMModel, r.TTSProvider, r.TTSModel)
		}
	}
	if f.format == "json" {
		return report.WriteJSON(a.out, rows)
	}
	return report.WriteCSV(a.out, report.Comment(prices.Version()), rows)
}

func (f reportFlags) validate() (from, to time.Time, err error) {
	switch f.format {
	case "csv", "json":
	default:
		return from, to, usagef("--format must be csv or json, got %q", f.format)
	}
	if strings.TrimSpace(f.from) == "" {
		return from, to, usagef("--from is required")
	}
	if strings.TrimSpace(f.to) == "" {
		return from, to, usagef("--to is required")
	}
	if from, err = parseReportTime(f.from); err != nil {
		return from, to, usagef("--from %q: want YYYY-MM-DD or RFC 3339", f.from)
	}
	if to, err = parseReportTime(f.to); err != nil {
		return from, to, usagef("--to %q: want YYYY-MM-DD or RFC 3339", f.to)
	}
	if !from.Before(to) {
		return from, to, usagef("--from (%s) must be before --to (%s): the range is [from, to)", f.from, f.to)
	}
	return from, to, nil
}

func parseReportTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if t, err := time.ParseInLocation(time.DateOnly, s, time.UTC); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339, s)
}
