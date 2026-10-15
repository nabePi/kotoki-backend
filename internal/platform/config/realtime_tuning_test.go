package config

import (
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/testutil"
)

func TestRealtimeTuningDefaults(t *testing.T) {
	r, err := loadRealtime(testutil.MinimalEnv(t))
	require.NoError(t, err)
	require.Equal(t, 100, r.WSBinaryRate)
	require.Equal(t, 200, r.WSBinaryBurst)
	require.Len(t, r.TurnFillerSuffixes, 14)
	require.Len(t, r.TurnCompletionSuffixes, 12)
	require.Equal(t, 3, r.MaxConsecutiveFailedTurns)
	require.Equal(t, 400, r.AudioMaxLeadMS)
	require.Equal(t, 4, r.RollingSummaryTriggerTurns)
	require.Equal(t, 10*time.Second, r.RollingSummaryTimeout)
	require.Equal(t, 30*time.Second, r.ReconnectWindow)
	require.Equal(t, 800, r.TurnSilenceDefaultMS)
	require.Equal(t, 500, r.TurnSilenceCompleteMS)
	require.Equal(t, "えっと", r.TurnFillerSuffixes[0])
	require.Equal(t, "。", r.TurnCompletionSuffixes[0])
}

func TestRealtimeTuningEnvOverride(t *testing.T) {
	r, err := loadRealtime(testutil.MinimalEnv(t, map[string]string{"ROLLING_SUMMARY_TRIGGER_TURNS": "6", "WS_BINARY_RATE": "50"}))
	require.NoError(t, err)
	require.Equal(t, 6, r.RollingSummaryTriggerTurns)
	require.Equal(t, 50, r.WSBinaryRate)
}

func TestRealtimeTuningValidate(t *testing.T) {
	for name, mut := range map[string]func(*RealtimeTuning){
		"complete>=default": func(c *RealtimeTuning) { c.TurnSilenceCompleteMS = c.TurnSilenceDefaultMS },
		"zero rate":         func(c *RealtimeTuning) { c.WSBinaryRate = 0 },
		"negative duration": func(c *RealtimeTuning) { c.ReconnectWindow = -time.Second },
		"zero failed turns": func(c *RealtimeTuning) { c.MaxConsecutiveFailedTurns = 0 },
	} {
		c := defaultRealtimeTuning()
		require.NoError(t, c.Validate(), name)
		mut(&c)
		require.Error(t, c.Validate(), name)
	}
}

func TestRealtimeTuningNoDuplicateFields(t *testing.T) {
	own := map[string]bool{}
	var walk func(reflect.Type, map[string]bool, reflect.Type)
	walk = func(rt reflect.Type, into map[string]bool, skip reflect.Type) {
		for i := range rt.NumField() {
			f := rt.Field(i)
			if f.Type == skip {
				continue
			}
			if f.Anonymous {
				walk(f.Type, into, skip)
				continue
			}
			into[f.Name] = true
			if tag := f.Tag.Get("env"); tag != "" {
				into["env:"+tag] = true
			}
		}
	}
	walk(reflect.TypeOf(RealtimeTuning{}), own, nil)
	base := map[string]bool{}
	walk(reflect.TypeOf(Realtime{}), base, reflect.TypeOf(RealtimeTuning{}))
	for k := range own {
		require.False(t, base[k], "RealtimeTuning duplicates Realtime field %s", k)
	}
}
