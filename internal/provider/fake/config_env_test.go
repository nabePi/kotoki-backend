package fake

import (
	"testing"
	"time"

	"github.com/caarlos0/env/v11"
	"github.com/stretchr/testify/require"
)

func TestConfigEnvDefaults(t *testing.T) {
	var c Config
	require.NoError(t, env.ParseWithOptions(&c, env.Options{Environment: map[string]string{}}))
	require.EqualValues(t, 42, c.Seed)
	require.Equal(t, 0.8, c.STTMatchThreshold)
	require.Equal(t, -40.0, c.STTEnergyDBFS)
	require.Len(t, c.STTPhrases, 5)
	require.Equal(t, "はい、そうです。", c.STTPhrases[0])
	require.Equal(t, 30, c.LLMTokenIntervalMS)
	require.Equal(t, 120, c.TTSMsPerChar)
	require.Equal(t, 5, c.TTSChunkIntervalMS)
	require.Equal(t, "tone", c.TTSWaveform)
	require.Equal(t, "kotoki:fake:faults", c.FaultsRedisKey)
	require.Equal(t, time.Second, c.FaultsPollInterval)
}

func TestConfigEnvOverrides(t *testing.T) {
	var c Config
	require.NoError(t, env.ParseWithOptions(&c, env.Options{Environment: map[string]string{
		"FAKE_SEED": "7", "FAKE_STT_PHRASES": "a|b", "FAKE_FAULTS_POLL_INTERVAL": "250ms",
	}}))
	require.EqualValues(t, 7, c.Seed)
	require.Equal(t, []string{"a", "b"}, c.STTPhrases)
	require.Equal(t, 250*time.Millisecond, c.FaultsPollInterval)
}
