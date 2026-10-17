package live_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/test/chaos/live"
	"github.com/nabePi/kotoki-backend/test/load/verify"
)

// TestMain doubles as the fake kotoki-load: the tests re-execute the test binary with
// KOTOKI_FAKE_LOAD set, which behaves like the generator (runs until SIGINT, then exits with the
// code the test asked for).
func TestMain(m *testing.M) {
	if mode := os.Getenv("KOTOKI_FAKE_LOAD"); mode != "" {
		fakeLoad(mode)
		return
	}
	os.Exit(m.Run())
}

func fakeLoad(mode string) {
	switch mode {
	case "exit2":
		_, _ = os.Stderr.WriteString("kotoki-load run: baseline scrape failed\n")
		os.Exit(2)
	case "ignore-int":
		ch := make(chan os.Signal, 1)
		signalNotify(ch)
		for range ch { // never exits on SIGINT: only a kill ends it
		}
	default:
		ch := make(chan os.Signal, 1)
		signalNotify(ch)
		<-ch
		switch mode {
		case "gate-fails":
			os.Exit(1)
		case "crash-on-stop":
			os.Exit(2)
		}
		_, _ = os.Stdout.WriteString("args: " + strings.Join(os.Args[1:], " ") + "\n")
		os.Exit(0)
	}
}

func loader(mode string, out *bytes.Buffer) live.Loader {
	return live.Loader{
		Bin: os.Args[0], Args: []string{"run", "--gate", "g1-local"}, Env: append(os.Environ(), "KOTOKI_FAKE_LOAD="+mode),
		Out: out, StartGrace: 300 * time.Millisecond, StopTimeout: 2 * time.Second,
	}
}

func TestLoader_StartsWithVUsAndStopsWithSIGINT(t *testing.T) {
	var out bytes.Buffer
	stop, err := loader("ok", &out).Start(t.Context(), 42)
	require.NoError(t, err)
	require.NoError(t, stop())
	require.Contains(t, out.String(), "args: run --gate g1-local --vus 42")
	require.NoError(t, stop(), "stop is idempotent")
}

func TestLoader_GateVerdictOfTheLoadIsNotAnError(t *testing.T) {
	// under injection the load's own gate is expected to fail (exit 1); the chaos assertions decide
	stop, err := loader("gate-fails", &bytes.Buffer{}).Start(t.Context(), 1)
	require.NoError(t, err)
	require.NoError(t, stop())
}

func TestLoader_SetupFailureStopsTheRunAtStart(t *testing.T) {
	var out bytes.Buffer
	_, err := loader("exit2", &out).Start(t.Context(), 1)
	require.Error(t, err)
	require.Contains(t, err.Error(), "exit status 2")
}

func TestLoader_CrashAtStopIsAnError(t *testing.T) {
	stop, err := loader("crash-on-stop", &bytes.Buffer{}).Start(t.Context(), 1)
	require.NoError(t, err)
	require.ErrorContains(t, stop(), "exit status 2")
}

func TestLoader_KillsAProcessThatIgnoresSIGINT(t *testing.T) {
	stop, err := loader("ignore-int", &bytes.Buffer{}).Start(t.Context(), 1)
	require.NoError(t, err)
	start := time.Now()
	require.Error(t, stop(), "a load that does not stop is an error")
	require.Less(t, time.Since(start), 8*time.Second)
}

func TestLoader_ContextCancelStopsTheProcess(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	stop, err := loader("ok", &bytes.Buffer{}).Start(ctx, 1)
	require.NoError(t, err)
	cancel()
	require.NoError(t, stop())
}

func TestLoader_MissingBinary(t *testing.T) {
	_, err := live.Loader{Bin: "/nonexistent/kotoki-load"}.Start(t.Context(), 1)
	require.Error(t, err)
}

// ---- verifier ---------------------------------------------------------------------------------

func TestVerifier_CountsViolationsAndPassesSince(t *testing.T) {
	var out bytes.Buffer
	var got verify.Options
	v := live.Verifier{DSN: "postgres://u:secret@h/db", Await: 3 * time.Minute, Out: &out,
		Check: func(_ context.Context, dsn string, o verify.Options) ([]verify.Violation, error) {
			got = o
			require.Equal(t, "postgres://u:secret@h/db", dsn)
			return []verify.Violation{{Check: "V5", Detail: "session_id=1"}, {Check: "V8", Detail: "session_id=2"}}, nil
		}}
	since := time.Date(2026, 10, 17, 1, 2, 3, 0, time.UTC)
	n, err := v.Run(t.Context(), since)
	require.NoError(t, err)
	require.Equal(t, 2, n)
	require.Equal(t, since, got.Since)
	require.Equal(t, 3*time.Minute, got.AwaitEvaluations)
	require.Contains(t, out.String(), "V5 session_id=1")
}

func TestVerifier_ErrorNeverEchoesTheDSN(t *testing.T) {
	v := live.Verifier{DSN: "postgres://u:secret@h/db",
		Check: func(context.Context, string, verify.Options) ([]verify.Violation, error) {
			return nil, errors.New("dial postgres://u:secret@h/db: refused")
		}}
	_, err := v.Run(t.Context(), time.Time{})
	require.Error(t, err)
	require.NotContains(t, err.Error(), "secret")
}

func TestVerifier_NeedsADSN(t *testing.T) {
	_, err := live.Verifier{}.Run(t.Context(), time.Time{})
	require.Error(t, err)
}
