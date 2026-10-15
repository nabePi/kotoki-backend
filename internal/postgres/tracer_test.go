package postgres

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

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

func TestSpanErrorIgnoresConstraintViolations(t *testing.T) {
	if spanError(&pgconn.PgError{Code: "23505"}) != nil {
		t.Error("a unique violation is an expected business outcome, not a span error")
	}
	if spanError(&pgconn.PgError{Code: "23503"}) != nil {
		t.Error("a foreign key violation is expected too")
	}
	if spanError(&pgconn.PgError{Code: "40001"}) == nil {
		t.Error("a serialization failure stays a span error")
	}
	if spanError(errors.New("boom")) == nil {
		t.Error("non-pg errors stay span errors")
	}
	if spanError(nil) != nil {
		t.Error("nil stays nil")
	}
}
