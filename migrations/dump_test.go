package migrations_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

var (
	partitionRE = regexp.MustCompile(`_p\d{6}`)
	itemSplitRE = regexp.MustCompile(`(?m)^--\n(-- Name: )`)
	volatileRE  = regexp.MustCompile(`(?m)^(-- Dumped (from|by) .*|\\(un)?restrict .*)\n`)
)

// normalizeSchemaDump removes everything that depends on the migration date
// (monthly partitions and their dependent items) or on the pg_dump build.
func normalizeSchemaDump(dump string) string {
	dump = volatileRE.ReplaceAllString(dump, "")
	idx := itemSplitRE.FindAllStringIndex(dump, -1)
	if len(idx) == 0 {
		return dump
	}
	var b strings.Builder
	b.WriteString(dump[:idx[0][0]])
	for i, loc := range idx {
		end := len(dump)
		if i+1 < len(idx) {
			end = idx[i+1][0]
		}
		item := dump[loc[0]:end]
		if !partitionRE.MatchString(item) {
			b.WriteString(item)
		}
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

func TestNormalizeDump(t *testing.T) {
	item := func(name, body string) string {
		return "--\n-- Name: " + name + "; Type: TABLE; Schema: public; Owner: -\n--\n\n" + body + "\n\n\n"
	}
	dump := "-- Dumped from database version 17.5\n-- Dumped by pg_dump version 17.5\n\\restrict abc123\n\nSET x = 1;\n\n" +
		item("session_turns", "CREATE TABLE public.session_turns (id uuid);") +
		item("session_turns_p202610", "CREATE TABLE public.session_turns_p202610 (id uuid);") +
		item("session_turns_p202701", "CREATE TABLE public.session_turns_p202701 (id uuid);") +
		item("session_turns_p202610 session_turns_p202610_pkey", "ALTER INDEX public.pk ATTACH PARTITION public.session_turns_p202610_pkey;") +
		item("users", "CREATE TABLE public.users (id uuid);")
	got := normalizeSchemaDump(dump)
	require.NotContains(t, got, "202610")
	require.NotContains(t, got, "202701")
	require.NotContains(t, got, "Dumped")
	require.NotContains(t, got, "restrict")
	require.Contains(t, got, "CREATE TABLE public.session_turns (id uuid);")
	require.Contains(t, got, "CREATE TABLE public.users (id uuid);")
	require.Contains(t, got, "SET x = 1;")
}
