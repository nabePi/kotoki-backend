//go:build !windows

package main

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/test/load/verify"
)

func fakeRun(vs []verify.Violation, err error, got *verify.Options) runFunc {
	return func(_ context.Context, _ string, o verify.Options) ([]verify.Violation, error) {
		*got = o
		return vs, err
	}
}

func TestMain_ExitCodes(t *testing.T) {
	var got verify.Options
	var out, errb bytes.Buffer

	// clean: 0
	require.Equal(t, 0, realMain(context.Background(), []string{"--db", "postgres://x/y", "--since", "2026-10-17T08:00:00Z"}, &out, &errb, fakeRun([]verify.Violation{}, nil, &got)))
	require.Contains(t, out.String(), "0 violations")
	require.True(t, got.Since.Equal(time.Date(2026, 10, 17, 8, 0, 0, 0, time.UTC)))
	require.Zero(t, got.AwaitEvaluations)

	// violations: 1, each one printed
	out.Reset()
	vs := []verify.Violation{{Check: "V1", Detail: "user_id=u available=1/2"}, {Check: "V3", Detail: "reservation_id=r"}}
	require.Equal(t, 1, realMain(context.Background(), []string{"--db", "postgres://x/y", "--await-evaluations", "5m"}, &out, &errb, fakeRun(vs, nil, &got)))
	require.Contains(t, out.String(), "V1")
	require.Contains(t, out.String(), "reservation_id=r")
	require.Contains(t, out.String(), "2 violations")
	require.Equal(t, 5*time.Minute, got.AwaitEvaluations)
	require.True(t, got.Since.IsZero())

	// the check itself failed: 2, never 0
	errb.Reset()
	require.Equal(t, 2, realMain(context.Background(), []string{"--db", "postgres://x/y"}, &out, &errb, fakeRun(nil, errors.New("boom"), &got)))
	require.Contains(t, errb.String(), "boom")

	// usage errors: 2
	for _, args := range [][]string{{}, {"--db", "x", "--since", "yesterday"}, {"--db", "x", "--await-evaluations", "-1s"}, {"--db", "x", "extra"}, {"--nope"}} {
		require.Equal(t, 2, realMain(context.Background(), args, &out, &errb, fakeRun([]verify.Violation{}, nil, &got)), args)
	}
}

func TestMain_DBURLIsNeverPrinted(t *testing.T) {
	var got verify.Options
	var out, errb bytes.Buffer
	realMain(context.Background(), []string{"--db", "postgres://user:s3cret@host/db"}, &out, &errb,
		fakeRun(nil, errors.New("dial postgres://user:s3cret@host/db: refused"), &got))
	require.Contains(t, errb.String(), "refused")
	require.NotContains(t, out.String()+errb.String(), "s3cret")
}
