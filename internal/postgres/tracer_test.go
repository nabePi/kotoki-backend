package postgres

import "testing"

func TestQueryName(t *testing.T) {
	cases := map[string]string{
		"-- name: InsertSession :one\nINSERT INTO sessions ...": "InsertSession",
		"-- name: Ping :one\nSELECT 1::int":                     "Ping",
		"SELECT 1":                                              "query",
		"":                                                      "query",
		"-- some comment\nSELECT 1":                             "query",
	}
	for sql, want := range cases {
		if got := queryName(sql); got != want {
			t.Errorf("queryName(%q) = %q, want %q", sql, got, want)
		}
	}
}
