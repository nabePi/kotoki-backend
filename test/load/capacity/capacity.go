// Package capacity is the capacity model of spec 13.11 as code: Default holds the behaviour
// assumptions, Derive turns them into the derived quantities of the spec table (pods, bandwidth,
// vendor quotas, connection budget, ...) and Validate compares measured values from a load run
// against that model. The model is pure arithmetic; it never invents a measurement.
package capacity

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"
	"text/tabwriter"
)

// Behaviour assumptions of spec 13.11 (same as the load model of 13.12.2).
const (
	defaultSessionSec    = 900  // 15 minutes
	defaultTurnsPerMin   = 4.0  // 15 s turn cycle
	podTarget            = 70   // HPA target sessions per pod
	podHardMax           = 100  // hard limit sessions per pod (D-103)
	opusKbps             = 32.0 // upstream payload
	framesPerSec         = 50.0 // Opus 20 ms frames, mic always open
	aiSpeakingShare      = 0.4  // share of time the AI talks
	textChars            = 60
	inputTokensPerTurn   = 2500
	outputTokensPerTurn  = 80
	sttPCMKbps           = 256.0 // PCM16 16 kHz
	sttBase64Factor      = 1.33  // base64 JSON framing
	llmStreamSec         = 2.5
	ttsGenerationSec     = 2.0
	podVCPURequest       = 2.0
	nodeAllocatableVCPU  = 7.5
	apiPodsPerCCU        = 400 // api pods = ceil(CCU/400): 3 pods at 1.000 CCU (13.3.2)
	apiMinPods           = 1
	workerCorePods       = 2
	evalSecPerSession    = 20.0
	memorySecPerSession  = 10.0
	learnerSecPerSession = 5.0
	workerPrefetch       = 4.0

	// pool sizes per pod (DB_MAX_CONNS, 13.3.2)
	poolRealtime, poolAPI, poolWorker = 6, 10, 4
	// migrate + operator connections
	poolMigrateOperator = 15
	// HPA maximums at the 13.3.2 table (a configuration value, not derived from CCU)
	maxRealtimePods, maxAPIPods, maxCorePods, maxEvalPods, maxMemoryPods, maxLearnerPods = 20, 6, 2, 8, 4, 3

	// PG write transactions: 2 per turn (insert+update) and about 8 per session lifecycle
	pgTxPerTurn, pgTxPerSessionLifecycle = 2.0, 8.0

	// Redis ops per second: per-session rates (heartbeat every 5 s, owner refresh every 5 s) and
	// per-CCU rates for mirror state, replay XADD+EXPIRE and lock/ticket/ratelimit (13.11)
	redisHeartbeatPerSession = 1.0 / 5
	redisOwnerPerSession     = 1.0 / 5
	redisMirrorPerSession    = 0.33
	redisReplayPerSession    = 2.7
	redisMiscPerSession      = 0.25
)

// Validation limits (Task 22).
const (
	maxCPUCoresPer100Sessions = 1.5   // <= 1,5% core per session
	modelMemMBPer100Sessions  = 300.0 // 2-3 MB per session
	memFactor                 = 1.3
	tolerance                 = 1.3 // "within 30% of the model, or smaller"
)

// Model is the set of behaviour assumptions.
type Model struct {
	CCU                   int
	AvgSessionSec         int
	TurnsPerMinPerSession float64
	PodTarget             int // sessions per pod the HPA aims at (70)
	PodHardMax            int // hard session limit per pod (100)
	OpusKbps              float64
	TextChars             int
	InputTokensPerTurn    int
	OutputTokensPerTurn   int
}

// Default is the model of spec 13.11 for ccu concurrent sessions.
func Default(ccu int) Model {
	return Model{CCU: ccu, AvgSessionSec: defaultSessionSec, TurnsPerMinPerSession: defaultTurnsPerMin,
		PodTarget: podTarget, PodHardMax: podHardMax, OpusKbps: opusKbps, TextChars: textChars,
		InputTokensPerTurn: inputTokensPerTurn, OutputTokensPerTurn: outputTokensPerTurn}
}

// Derived is the capacity table of spec 13.11 for one Model.
type Derived struct {
	TurnsPerSec           float64
	SessionStartsPerSec   float64
	RealtimePods          int
	RealtimePodsAtHardMax int
	RealtimeNodes         int
	FramesInPerSec        float64
	FramesOutPerSec       float64
	STTEgressMbps         float64
	LLMRPM                int
	LLMInputTPM           float64
	LLMOutputTPM          float64
	LLMStreams            int
	TTSStreams            int
	TTSCharsPerMin        float64
	PGTxPerSec            float64
	PGConnsAt1000         int // pool budget at this CCU (the name follows the spec table, 13.3.2)
	PGConnsAtMax          int // pool budget with every Deployment at its HPA maximum
	RedisOpsPerSec        float64
	EvalPods              int
	MemoryPods            int
	LearnerPods           int
}

func ceilDiv(a, b float64) int { return int(math.Ceil(a/b - 1e-9)) }

func (m Model) turnsPerSec() float64 { return float64(m.CCU) * m.TurnsPerMinPerSession / 60 }

func (m Model) startsPerSec() float64 { return float64(m.CCU) / float64(m.AvgSessionSec) }

func (m Model) realtimePods() int { return ceilDiv(float64(m.CCU), float64(m.PodTarget)) }

func (m Model) realtimePodsAtHardMax() int { return ceilDiv(float64(m.CCU), float64(m.PodHardMax)) }

func (m Model) realtimeNodes() int {
	perNode := int(math.Floor(nodeAllocatableVCPU / podVCPURequest)) // whole pods that fit one node
	return ceilDiv(float64(m.realtimePods()), float64(perNode))
}

func (m Model) framesIn() float64 { return framesPerSec * float64(m.CCU) }

func (m Model) framesOut() float64 { return m.framesIn() * aiSpeakingShare }

func (m Model) sttEgressMbps() float64 { return sttPCMKbps * sttBase64Factor * float64(m.CCU) / 1000 }

func (m Model) llmRPM() int { return int(math.Round(m.turnsPerSec() * 60)) }

func (m Model) llmStreams() int { return int(math.Round(m.turnsPerSec() * llmStreamSec)) }

func (m Model) ttsStreams() int { return int(math.Round(m.turnsPerSec() * ttsGenerationSec)) }

func (m Model) ttsCharsPerMin() float64 { return m.turnsPerSec() * 60 * float64(m.TextChars) }

func (m Model) pgTxPerSec() float64 {
	return m.turnsPerSec()*pgTxPerTurn + m.startsPerSec()*pgTxPerSessionLifecycle
}

func (m Model) workerPods(secPerSession float64) int {
	return ceilDiv(m.startsPerSec()*secPerSession, workerPrefetch)
}

func (m Model) apiPods() int {
	if m.CCU <= 0 {
		return 0
	}
	return max(apiMinPods, ceilDiv(float64(m.CCU), apiPodsPerCCU))
}

func (m Model) pgConns() int {
	if m.CCU <= 0 {
		return 0
	}
	return m.realtimePods()*poolRealtime + m.apiPods()*poolAPI + workerCorePods*poolWorker +
		m.workerPods(evalSecPerSession)*poolWorker + m.workerPods(memorySecPerSession)*poolWorker +
		m.workerPods(learnerSecPerSession)*poolWorker + poolMigrateOperator
}

func pgConnsAtMax() int {
	return maxRealtimePods*poolRealtime + maxAPIPods*poolAPI + (maxCorePods+maxEvalPods+maxMemoryPods+maxLearnerPods)*poolWorker +
		poolMigrateOperator
}

func (m Model) redisOps() float64 {
	perSession := redisHeartbeatPerSession + redisOwnerPerSession + redisMirrorPerSession + redisReplayPerSession + redisMiscPerSession
	return perSession * float64(m.CCU)
}

// Derive computes the table of spec 13.11.
func (m Model) Derive() Derived {
	return Derived{
		TurnsPerSec:           m.turnsPerSec(),
		SessionStartsPerSec:   m.startsPerSec(),
		RealtimePods:          m.realtimePods(),
		RealtimePodsAtHardMax: m.realtimePodsAtHardMax(),
		RealtimeNodes:         m.realtimeNodes(),
		FramesInPerSec:        m.framesIn(),
		FramesOutPerSec:       m.framesOut(),
		STTEgressMbps:         m.sttEgressMbps(),
		LLMRPM:                m.llmRPM(),
		LLMInputTPM:           m.turnsPerSec() * 60 * float64(m.InputTokensPerTurn),
		LLMOutputTPM:          m.turnsPerSec() * 60 * float64(m.OutputTokensPerTurn),
		LLMStreams:            m.llmStreams(),
		TTSStreams:            m.ttsStreams(),
		TTSCharsPerMin:        m.ttsCharsPerMin(),
		PGTxPerSec:            m.pgTxPerSec(),
		PGConnsAt1000:         m.pgConns(),
		PGConnsAtMax:          pgConnsAtMax(),
		RedisOpsPerSec:        m.redisOps(),
		EvalPods:              m.workerPods(evalSecPerSession),
		MemoryPods:            m.workerPods(memorySecPerSession),
		LearnerPods:           m.workerPods(learnerSecPerSession),
	}
}

// Measured holds values taken from a load run (G4a/G4b); the caller fills them from real
// measurements. NaN or a negative value means "not measured".
type Measured struct {
	CPUCoresPer100Sessions float64
	MemMBPer100Sessions    float64
	STTEgressMbps          float64
	PGTxPerSec             float64
	RedisOps               float64
}

// Metric names of Finding.Metric.
const (
	MetricCPU      = "realtime_cpu_cores_per_100_sessions"
	MetricMem      = "realtime_mem_mb_per_100_sessions"
	MetricSTT      = "stt_egress_mbps"
	MetricPGTx     = "pg_tx_per_sec"
	MetricRedisOps = "redis_ops_per_sec" //nolint:gosec // G101: false positive, a metric name
)

// Finding is one measured-vs-model comparison. Model is the limit the measurement is held to.
type Finding struct {
	Metric          string
	Model, Measured float64
	OK              bool
}

func measured(v float64) bool { return !math.IsNaN(v) && v >= 0 }

func finding(metric string, model, got float64, limit float64) Finding {
	return Finding{Metric: metric, Model: model, Measured: got, OK: measured(got) && got <= limit}
}

// Validate compares a measurement with the model: CPU at most 1,5 core per 100 sessions, memory at
// most 1,3 x 300 MB per 100 sessions, every other value within +30% of the model or smaller. A value
// that was not measured (NaN, negative) never passes.
func Validate(d Derived, m Measured) []Finding {
	return []Finding{
		finding(MetricCPU, maxCPUCoresPer100Sessions, m.CPUCoresPer100Sessions, maxCPUCoresPer100Sessions),
		finding(MetricMem, modelMemMBPer100Sessions, m.MemMBPer100Sessions, modelMemMBPer100Sessions*memFactor),
		finding(MetricSTT, d.STTEgressMbps, m.STTEgressMbps, d.STTEgressMbps*tolerance),
		finding(MetricPGTx, d.PGTxPerSec, m.PGTxPerSec, d.PGTxPerSec*tolerance),
		finding(MetricRedisOps, d.RedisOpsPerSec, m.RedisOps, d.RedisOpsPerSec*tolerance),
	}
}

// AllOK is true when there is at least one finding and every one is OK.
func AllOK(fs []Finding) bool {
	if len(fs) == 0 {
		return false
	}
	for _, f := range fs {
		if !f.OK {
			return false
		}
	}
	return true
}

// LoadMeasured reads a measurement file. Every key is required (a missing key is "not measured",
// not 0) and unknown keys are rejected.
func LoadMeasured(path string) (Measured, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // operator-supplied path
	if err != nil {
		return Measured{}, fmt.Errorf("capacity: read measured file: %w", err)
	}
	var in struct {
		CPU   *float64 `json:"cpu_cores_per_100_sessions"`
		Mem   *float64 `json:"mem_mb_per_100_sessions"`
		STT   *float64 `json:"stt_egress_mbps"`
		PGTx  *float64 `json:"pg_tx_per_sec"`
		Redis *float64 `json:"redis_ops_per_sec"`
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return Measured{}, fmt.Errorf("capacity: decode %s: %w", path, err)
	}
	if dec.More() {
		return Measured{}, fmt.Errorf("capacity: %s: trailing data after the JSON object", path)
	}
	for _, kv := range []struct {
		key string
		v   *float64
	}{
		{"cpu_cores_per_100_sessions", in.CPU}, {"mem_mb_per_100_sessions", in.Mem}, {"stt_egress_mbps", in.STT},
		{"pg_tx_per_sec", in.PGTx}, {"redis_ops_per_sec", in.Redis},
	} {
		if kv.v == nil {
			return Measured{}, fmt.Errorf("capacity: %s: missing key %q", path, kv.key)
		}
	}
	return Measured{CPUCoresPer100Sessions: *in.CPU, MemMBPer100Sessions: *in.Mem, STTEgressMbps: *in.STT,
		PGTxPerSec: *in.PGTx, RedisOps: *in.Redis}, nil
}

// RenderDerived renders the derived table as aligned text.
func RenderDerived(ccu int, d Derived) string {
	var b strings.Builder
	fmt.Fprintf(&b, "capacity model (spec 13.11) for %d CCU\n", ccu)
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	row := func(name, format string, v ...any) {
		_, _ = fmt.Fprintf(tw, "%s\t"+format+"\n", append([]any{name}, v...)...)
	}
	row("turns/s", "%.1f", d.TurnsPerSec)
	row("session starts/s", "%.2f", d.SessionStartsPerSec)
	row("realtime pods", "%d", d.RealtimePods)
	row("realtime pods at hard max", "%d", d.RealtimePodsAtHardMax)
	row("realtime nodes", "%d", d.RealtimeNodes)
	row("frames in/s", "%.0f", d.FramesInPerSec)
	row("frames out/s", "%.0f", d.FramesOutPerSec)
	row("STT egress Mbps", "%.0f", d.STTEgressMbps)
	row("LLM RPM", "%d", d.LLMRPM)
	row("LLM input TPM", "%.0f", d.LLMInputTPM)
	row("LLM output TPM", "%.0f", d.LLMOutputTPM)
	row("LLM streams", "%d", d.LLMStreams)
	row("TTS streams", "%d", d.TTSStreams)
	row("TTS chars/min", "%.0f", d.TTSCharsPerMin)
	row("PG write tx/s", "%.0f", d.PGTxPerSec)
	row("PG connections (pool budget)", "%d", d.PGConnsAt1000)
	row("PG connections at HPA max", "%d", d.PGConnsAtMax)
	row("Redis ops/s", "%.0f", d.RedisOpsPerSec)
	row("eval worker pods", "%d", d.EvalPods)
	row("memory worker pods", "%d", d.MemoryPods)
	row("learner worker pods", "%d", d.LearnerPods)
	_ = tw.Flush()
	return b.String()
}

// RenderFindings renders the measured-vs-model comparison.
func RenderFindings(fs []Finding) string {
	var b strings.Builder
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "metric\tlimit\tmeasured\tresult")
	for _, f := range fs {
		res := "OK"
		if !f.OK {
			res = "FAIL"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%.2f\t%.2f\t%s\n", f.Metric, f.Model, f.Measured, res)
	}
	_ = tw.Flush()
	return b.String()
}
