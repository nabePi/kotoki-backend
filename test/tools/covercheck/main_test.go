package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// profile builds a coverage profile where pkg has total statements and covered of them executed.
func profile(pkg string, total, covered int) string {
	var b strings.Builder
	b.WriteString("mode: atomic\n")
	for i := 0; i < total; i++ {
		c := 0
		if i < covered {
			c = 1
		}
		fmt.Fprintf(&b, "github.com/nabePi/kotoki-backend/%s/f.go:%d.1,%d.10 1 %d\n", pkg, i+1, i+1, c)
	}
	return b.String()
}

const thresholds = "internal/balance 90\nTOTAL internal 75\n"

func TestThresholdFailsBelow(t *testing.T) {
	fails, err := check(strings.NewReader(profile("internal/balance", 100, 80)), strings.NewReader(thresholds), nil)
	require.NoError(t, err)
	require.Len(t, fails, 1) // 80% total still clears TOTAL 75
	require.Contains(t, strings.Join(fails, "\n"), "internal/balance")
}

func TestThresholdPassesAbove(t *testing.T) {
	fails, err := check(strings.NewReader(profile("internal/balance", 100, 95)), strings.NewReader(thresholds), nil)
	require.NoError(t, err)
	require.Empty(t, fails)
}

func TestPackageWithoutDataIgnored(t *testing.T) {
	fails, err := check(strings.NewReader(profile("internal/other", 100, 100)), strings.NewReader(thresholds), nil)
	require.NoError(t, err)
	require.Empty(t, fails)
}

func TestBaselineDrop(t *testing.T) {
	th := strings.NewReader("")
	base := profile("internal/x", 200, 200)
	fails, err := check(strings.NewReader(profile("internal/x", 200, 197)), th, strings.NewReader(base)) // -1.5
	require.NoError(t, err)
	require.Len(t, fails, 1)
	fails, err = check(strings.NewReader(profile("internal/x", 200, 199)), strings.NewReader(""), strings.NewReader(base)) // -0.5
	require.NoError(t, err)
	require.Empty(t, fails)
}

func TestTotalExcludesSqlcgenAndTestutil(t *testing.T) {
	p := profile("internal/balance", 100, 95) + strings.TrimPrefix(profile("internal/postgres/sqlcgen", 1000, 0), "mode: atomic\n") +
		strings.TrimPrefix(profile("internal/testutil", 1000, 0), "mode: atomic\n")
	fails, err := check(strings.NewReader(p), strings.NewReader(thresholds), nil)
	require.NoError(t, err)
	require.Empty(t, fails)
}
