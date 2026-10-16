//go:build integration

package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/testutil"
)

const reportPrices = `version: "2026-10-17"
currency: USD
entries:
  - {provider: OPENAI,     capability: LLM, model: m1, input_per_1m_tokens: 1, output_per_1m_tokens: 2}
  - {provider: ELEVENLABS, capability: TTS, model: "*", per_1m_characters: 10}
  - {provider: OPENAI,     capability: STT, model: "*", per_audio_hour: 0.6}
`

// TestProviderReportEndToEnd runs the whole subcommand against a migrated database: the read-only
// connection, the query, the prices file and both output formats.
func TestProviderReportEndToEnd(t *testing.T) {
	pool := testutil.NewPostgres(t)
	ctx := context.Background()
	conn, err := pool.Acquire(ctx)
	require.NoError(t, err)
	_, err = conn.Exec(ctx, `SET session_replication_role = replica`)
	require.NoError(t, err)
	now := time.Now().UTC()
	at := time.Date(now.Year(), now.Month(), 3, 12, 0, 0, 0, time.UTC)
	for i, st := range []string{"COMPLETED", "COMPLETED", "FAILED", "DISCARDED"} {
		_, err = conn.Exec(ctx, `INSERT INTO session_turns (id, session_id, sequence_number, status, generation, stt_provider, llm_provider,
			llm_model, tts_provider, tts_model, turn_end_to_first_audio_ms, input_tokens, output_tokens, tts_characters, stt_audio_ms, created_at, completed_at)
			VALUES ($1, $2, $3, $4, 1, 'OPENAI', 'OPENAI', 'm1', 'ELEVENLABS', 't1', 500, 1000, 100, 50, 30000, $5, $5)`,
			uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), i+1, st, at)
		require.NoError(t, err)
	}
	conn.Release()
	prices := filepath.Join(t.TempDir(), "prices.yaml")
	require.NoError(t, os.WriteFile(prices, []byte(reportPrices), 0o600))
	env := map[string]string{"DATABASE_URL": pool.Config().ConnString()}
	from, to := at.AddDate(0, 0, -1).Format("2006-01-02"), at.AddDate(0, 0, 1).Format("2006-01-02")

	code, out, errw := runReport(t, env, "--from", from, "--to", to, "--prices", prices)
	require.Equal(t, 0, code, errw)
	assert.Empty(t, errw, "everything is priced")
	r := csv.NewReader(strings.NewReader(out))
	r.Comment = '#'
	recs, err := r.ReadAll()
	require.NoError(t, err)
	require.Len(t, recs, 2, "header + one stack (the DISCARDED turn does not count)")
	got := map[string]string{}
	for i, c := range recs[0] {
		got[c] = recs[1][i]
	}
	assert.Equal(t, "3", got["turns"])
	assert.Equal(t, "0.3333", got["failed_rate"])
	assert.Equal(t, "500", got["p95_turn_end_to_first_audio_ms"])
	total := (3*1000.0/1e6*1 + 3*100.0/1e6*2) + 3*50.0/1e6*10 + 90000.0/3.6e6*0.6
	assert.InDelta(t, total/3, mustFloat(t, got["avg_cost_usd_per_turn"]), 1e-6)
	assert.InDelta(t, total/1.5, mustFloat(t, got["cost_usd_per_active_minute"]), 1e-6, "90 s of STT audio = 1.5 active minutes")
	assert.Equal(t, "", got["avg_eval_overall_score"])
	assert.Contains(t, out, "prices version 2026-10-17")

	code, out, errw = runReport(t, env, "--from", from, "--to", to, "--prices", prices, "--format", "json")
	require.Equal(t, 0, code, errw)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &rows))
	require.Len(t, rows, 1)
	assert.EqualValues(t, 3, rows[0]["turns"])

	// the embedded table has no STT wildcard: the cost still comes out, with a warning naming the gap
	code, out, errw = runReport(t, env, "--from", from, "--to", to)
	require.Equal(t, 0, code, errw)
	assert.Contains(t, errw, "no price for")
	assert.Contains(t, out, "OPENAI,OPENAI,m1,ELEVENLABS,t1,3,")

	// an empty range prints just the header
	code, out, errw = runReport(t, env, "--from", "2001-01-01", "--to", "2001-01-02", "--prices", prices)
	require.Equal(t, 0, code, errw)
	r = csv.NewReader(strings.NewReader(out))
	r.Comment = '#'
	recs, err = r.ReadAll()
	require.NoError(t, err)
	assert.Len(t, recs, 1)
}

func mustFloat(t *testing.T, s string) float64 {
	t.Helper()
	var f float64
	require.NoError(t, json.Unmarshal([]byte(s), &f), s)
	return f
}
