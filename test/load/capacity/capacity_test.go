package capacity

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/test/load/gate"
)

func TestDerive_At1000CCU(t *testing.T) {
	d := Default(1000).Derive()
	require.InDelta(t, 66.7, d.TurnsPerSec, 0.05)
	require.InDelta(t, 1.1, d.SessionStartsPerSec, 0.02)
	require.Equal(t, 15, d.RealtimePods)
	require.Equal(t, 10, d.RealtimePodsAtHardMax)
	require.Equal(t, 5, d.RealtimeNodes)
	require.InDelta(t, 50000, d.FramesInPerSec, 0.001)
	require.InDelta(t, 20000, d.FramesOutPerSec, 0.001)
	require.InDelta(t, 341, d.STTEgressMbps, 2)
	require.Equal(t, 4000, d.LLMRPM)
	require.InDelta(t, 135, float64(d.TTSStreams), 5)
	require.InDelta(t, 170, float64(d.LLMStreams), 5)
	require.Equal(t, 187, d.PGConnsAt1000)
	require.Equal(t, 263, d.PGConnsAtMax)
	require.InDelta(t, 145, d.PGTxPerSec, 5)
	require.InDelta(t, 3700, d.RedisOpsPerSec, 50)
	require.Equal(t, 6, d.EvalPods)
	require.Equal(t, 3, d.MemoryPods)
	require.Equal(t, 2, d.LearnerPods)
}

func TestDerive_ScalesLinearly(t *testing.T) {
	d := Default(500).Derive()
	require.Equal(t, 8, d.RealtimePods)
	require.Equal(t, 5, d.RealtimePodsAtHardMax)
	require.InDelta(t, 25000, d.FramesInPerSec, 0.001)
	require.InDelta(t, 33.3, d.TurnsPerSec, 0.05)
	require.Equal(t, 2000, d.LLMRPM)
	// the pool budget at 500 CCU is smaller than at 1000
	require.Less(t, d.PGConnsAt1000, 187)
}

func TestDerive_PodTargetRoundsUp(t *testing.T) {
	require.Equal(t, 1, Default(1).Derive().RealtimePods)
	require.Equal(t, 1, Default(70).Derive().RealtimePods)
	require.Equal(t, 2, Default(71).Derive().RealtimePods)
	require.Equal(t, 1, Default(100).Derive().RealtimePodsAtHardMax)
	require.Equal(t, 2, Default(101).Derive().RealtimePodsAtHardMax)
}

func TestDerive_ZeroCCUIsZero(t *testing.T) {
	d := Default(0).Derive()
	require.Zero(t, d.RealtimePods)
	require.Zero(t, d.LLMRPM)
	require.Zero(t, d.RealtimeNodes)
}

// within returns a Measured exactly at the model for d.
func within(d Derived) Measured {
	return Measured{CPUCoresPer100Sessions: 1.2, MemMBPer100Sessions: 300,
		STTEgressMbps: d.STTEgressMbps, PGTxPerSec: d.PGTxPerSec, RedisOps: d.RedisOpsPerSec}
}

func find(t *testing.T, fs []Finding, metric string) Finding {
	t.Helper()
	for _, f := range fs {
		if f.Metric == metric {
			return f
		}
	}
	require.Failf(t, "finding missing", "%s in %+v", metric, fs)
	return Finding{}
}

func TestValidate_AllWithinModel(t *testing.T) {
	d := Default(1000).Derive()
	fs := Validate(d, within(d))
	require.Len(t, fs, 5)
	for _, f := range fs {
		require.True(t, f.OK, "%+v", f)
	}
}

func TestValidate_FlagsCPUOver(t *testing.T) {
	d := Default(1000).Derive()
	m := within(d)
	m.CPUCoresPer100Sessions = 1.6
	fs := Validate(d, m)
	require.False(t, find(t, fs, MetricCPU).OK)
	m.CPUCoresPer100Sessions = 1.5 // boundary is inclusive
	require.True(t, find(t, Validate(d, m), MetricCPU).OK)
}

func TestValidate_MemoryLimit(t *testing.T) {
	d := Default(1000).Derive()
	m := within(d)
	m.MemMBPer100Sessions = 390 // 1.3 x 300
	require.True(t, find(t, Validate(d, m), MetricMem).OK)
	m.MemMBPer100Sessions = 391
	require.False(t, find(t, Validate(d, m), MetricMem).OK)
}

func TestValidate_ToleranceIsThirtyPercentOrLess(t *testing.T) {
	d := Default(1000).Derive()
	for name, set := range map[string]func(*Measured, float64){
		MetricSTT:      func(m *Measured, v float64) { m.STTEgressMbps = v },
		MetricPGTx:     func(m *Measured, v float64) { m.PGTxPerSec = v },
		MetricRedisOps: func(m *Measured, v float64) { m.RedisOps = v },
	} {
		model := find(t, Validate(d, within(d)), name).Model
		m := within(d)
		set(&m, model*1.3)
		require.True(t, find(t, Validate(d, m), name).OK, name+" at +30%")
		set(&m, model*1.31)
		require.False(t, find(t, Validate(d, m), name).OK, name+" at +31%")
		set(&m, model*0.1) // much lower than the model is fine
		require.True(t, find(t, Validate(d, m), name).OK, name+" below model")
	}
}

func TestValidate_NotMeasuredNeverPasses(t *testing.T) {
	d := Default(1000).Derive()
	for _, bad := range []float64{math.NaN(), -1} {
		m := Measured{bad, bad, bad, bad, bad}
		for _, f := range Validate(d, m) {
			require.False(t, f.OK, "%+v", f)
		}
	}
}

func TestLoadMeasured(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) string {
		p := filepath.Join(dir, "m.json")
		require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
		return p
	}
	full := `{"cpu_cores_per_100_sessions":1.1,"mem_mb_per_100_sessions":280,"stt_egress_mbps":300,"pg_tx_per_sec":120,"redis_ops_per_sec":3000}`
	m, err := LoadMeasured(write(full))
	require.NoError(t, err)
	require.Equal(t, Measured{1.1, 280, 300, 120, 3000}, m)

	// a missing key is "not measured", never 0
	_, err = LoadMeasured(write(`{"cpu_cores_per_100_sessions":1.1}`))
	require.ErrorContains(t, err, "mem_mb_per_100_sessions")
	// unknown key (typo) is an error
	_, err = LoadMeasured(write(strings.Replace(full, "pg_tx_per_sec", "pg_tx", 1)))
	require.Error(t, err)
	_, err = LoadMeasured(write(strings.Replace(full, "}", `,"extra":1}`, 1)))
	require.ErrorContains(t, err, "extra")
	_, err = LoadMeasured(write(full + " trailing"))
	require.Error(t, err)
	_, err = LoadMeasured(filepath.Join(dir, "absent.json"))
	require.Error(t, err)
}

func TestRender(t *testing.T) {
	d := Default(1000).Derive()
	out := RenderDerived(1000, d)
	require.Contains(t, out, "1000")
	require.Contains(t, out, "realtime pods")
	require.Regexp(t, `(?m)^realtime pods\s+15\b`, out)

	fs := Validate(d, within(d))
	rep := RenderFindings(fs)
	require.Contains(t, rep, "OK")
	m := within(d)
	m.CPUCoresPer100Sessions = 2
	require.Contains(t, RenderFindings(Validate(d, m)), "FAIL")
	require.True(t, AllOK(fs))
	require.False(t, AllOK(Validate(d, m)))
	require.False(t, AllOK(nil))
}

// The G4a gate runs the 1.000 CCU of the model on realtime pods held at the hard max, so its
// VU count must be exactly what RealtimePodsAtHardMax pods hold (spec 13.12.3, K10 pod balance).
func TestGates_AgreeWithModel(t *testing.T) {
	m := Default(gate.Specs["g4a"].VUs)
	require.Equal(t, 10, m.Derive().RealtimePodsAtHardMax)
	require.Equal(t, gate.Specs["g4a"].VUs, m.Derive().RealtimePodsAtHardMax*m.PodHardMax)
	require.True(t, gate.Specs["g4a"].PodBalance)
	require.Equal(t, gate.Specs["g4a"].VUs, gate.Specs["g4b"].VUs)
	// G4b runs at the HPA target: 15 pods at 1.000 CCU
	require.Equal(t, 15, Default(gate.Specs["g4b"].VUs).Derive().RealtimePods)
	// G1..G3 and soak stay within what load generators (<= 500 VU each) and pods can take
	for _, name := range []string{"g1", "g2", "g3", "soak"} {
		require.LessOrEqual(t, gate.Specs[name].VUs, gate.Specs["g4a"].VUs, name)
	}
}
