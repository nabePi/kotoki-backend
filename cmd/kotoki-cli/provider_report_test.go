package main

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runReport runs the CLI with a controlled environment (no real DATABASE_URL leaks in).
func runReport(t *testing.T, env map[string]string, args ...string) (code int, out, errw string) {
	t.Helper()
	var o, e bytes.Buffer
	code = run(context.Background(), append([]string{"provider-report"}, args...), &o, &e, func(k string) string { return env[k] })
	return code, o.String(), e.String()
}

func TestProviderReportFlags(t *testing.T) {
	db := map[string]string{"DATABASE_URL": "postgres://u:p@127.0.0.1:1/none"}
	for name, tc := range map[string]struct {
		env  map[string]string
		args []string
		want string
	}{
		"from after to":      {db, []string{"--from", "2026-10-08", "--to", "2026-10-01"}, "--from"},
		"from equals to":     {db, []string{"--from", "2026-10-08", "--to", "2026-10-08"}, "--from"},
		"missing from":       {db, []string{"--to", "2026-10-08"}, "--from"},
		"missing to":         {db, []string{"--from", "2026-10-01"}, "--to"},
		"bad from":           {db, []string{"--from", "yesterday", "--to", "2026-10-08"}, "--from"},
		"bad to":             {db, []string{"--from", "2026-10-01", "--to", "10/08"}, "--to"},
		"bad format":         {db, []string{"--from", "2026-10-01", "--to", "2026-10-08", "--format", "xml"}, "--format"},
		"no database url":    {nil, []string{"--from", "2026-10-01", "--to", "2026-10-08"}, "DATABASE_URL"},
		"positional":         {db, []string{"--from", "2026-10-01", "--to", "2026-10-08", "extra"}, "unexpected argument"},
		"empty database url": {map[string]string{"DATABASE_URL": "", "DB_URL": ""}, []string{"--from", "2026-10-01", "--to", "2026-10-08"}, "DATABASE_URL"},
	} {
		t.Run(name, func(t *testing.T) {
			code, out, errw := runReport(t, tc.env, tc.args...)
			assert.Equal(t, 2, code, errw)
			assert.Empty(t, out)
			assert.Contains(t, errw, tc.want)
		})
	}
}

func TestProviderReportFlagsAreCheckedBeforeTheDatabase(t *testing.T) {
	// a bad range is a usage error even when the database does not answer: no connection is tried
	code, _, errw := runReport(t, map[string]string{"DATABASE_URL": "postgres://u:p@127.0.0.1:1/none"}, "--from", "2026-10-08", "--to", "2026-10-01")
	require.Equal(t, 2, code)
	assert.NotContains(t, errw, "connect")
}

func TestProviderReportBadPricesFile(t *testing.T) {
	code, out, errw := runReport(t, map[string]string{"DATABASE_URL": "postgres://u:p@127.0.0.1:1/none"},
		"--from", "2026-10-01", "--to", "2026-10-08", "--prices", "/nonexistent/prices.yaml")
	assert.Equal(t, 1, code)
	assert.Empty(t, out)
	assert.Contains(t, errw, "prices")
}

func TestProviderReportDoesNotLeakDatabasePassword(t *testing.T) {
	code, out, errw := runReport(t, map[string]string{"DATABASE_URL": "postgres://u:s3cretpw@127.0.0.1:1/none?connect_timeout=1"},
		"--from", "2026-10-01", "--to", "2026-10-08")
	assert.Equal(t, 1, code)
	assert.Empty(t, out)
	assert.NotContains(t, errw, "s3cretpw")
}

func TestProviderReportInUsage(t *testing.T) {
	_, _, usage := runBench(t, "help")
	assert.Contains(t, usage, "provider-report")
}
