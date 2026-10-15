//go:build e2e

package e2e

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/conversation/protocol"
	"github.com/nabePi/kotoki-backend/internal/kotokiclient"
	kcaudio "github.com/nabePi/kotoki-backend/internal/kotokiclient/audio"
)

const (
	// eventually is the longest any condition is awaited, polled every 50 ms.
	eventuallyWait = 10 * time.Second
	eventuallyTick = 50 * time.Millisecond
	// scriptTimeout bounds one whole scripted conversation.
	scriptTimeout  = 5 * time.Minute
	strongPassword = "kotoki-e2e-Passw0rd!x7"
)

// user is a registered, logged-in test user.
type user struct {
	ID     uuid.UUID
	Email  string
	Client *kotokiclient.Client
}

// ctxT returns a context that ends with the test.
func ctxT(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), d)
	t.Cleanup(cancel)
	return ctx
}

// newUser registers and logs in a fresh user over the real REST API. The per-IP registration
// limit (5/hour, spec 13.4.3) would stop a suite that runs from one address, so the limiter's
// bucket in the test Redis is cleared first; the limiter itself is covered elsewhere.
func newUser(t *testing.T) *user {
	t.Helper()
	ctx := ctxT(t, 30*time.Second)
	clearRateLimit(t, "auth_register_ip")
	c := kotokiclient.New(stack.apiURL)
	terms, err := c.Terms(ctx)
	require.NoError(t, err)
	email := fmt.Sprintf("e2e-%s@example.com", strings.ReplaceAll(uuid.NewString(), "-", ""))
	u, err := c.Register(ctx, kotokiclient.RegisterRequest{
		Email: email, Password: strongPassword, FullName: "E2E User", TermsVersion: terms,
	})
	require.NoError(t, err)
	require.NoError(t, c.Login(ctx, email, strongPassword))
	id, err := uuid.Parse(u.ID)
	require.NoError(t, err)
	return &user{ID: id, Email: email, Client: c}
}

// clearRateLimit deletes the Redis buckets of one limiter policy.
func clearRateLimit(t *testing.T, policy string) {
	t.Helper()
	ctx := ctxT(t, 5*time.Second)
	keys, err := stack.rdb.Keys(ctx, "kotoki:ratelimit:"+policy+":*").Result()
	require.NoError(t, err)
	if len(keys) > 0 {
		require.NoError(t, stack.rdb.Del(ctx, keys...).Err())
	}
}

// catalogChoice is the content every session of the suite is created from.
type catalogChoice struct {
	topic, scenario, character uuid.UUID
}

func (e *env) choose(t *testing.T, u *user) catalogChoice {
	t.Helper()
	e.catalogMu.Lock()
	defer e.catalogMu.Unlock()
	if e.catalog == nil {
		cat, err := u.Client.Catalog(ctxT(t, 30*time.Second))
		require.NoError(t, err)
		require.NotEmpty(t, cat.Scenarios)
		require.NotEmpty(t, cat.Characters)
		e.catalog = &catalogChoice{topic: cat.Scenarios[0].TopicID, scenario: cat.Scenarios[0].ID, character: cat.Characters[0].ID}
	}
	return *e.catalog
}

// startSession creates a 300 s RESERVED session in the given start mode (USER_FIRST or AI_FIRST).
func startSession(t *testing.T, u *user, mode string) kotokiclient.Session {
	t.Helper()
	return startSessionFor(t, u, mode, 300)
}

// startSessionFor is startSession with a chosen length in seconds.
func startSessionFor(t *testing.T, u *user, mode string, seconds int) kotokiclient.Session {
	t.Helper()
	c := stack.choose(t, u)
	s, err := u.Client.CreateSession(ctxT(t, 30*time.Second), kotokiclient.CreateSessionRequest{
		TopicID: c.topic, ScenarioID: c.scenario, CharacterID: c.character,
		DurationSeconds: seconds, ConversationStartMode: mode,
	})
	require.NoError(t, err)
	require.Equal(t, "RESERVED", s.Status)
	return s
}

var (
	packetsMu    sync.Mutex
	packetsCache = map[string][][]byte{}
)

// packets returns the Opus packets (48 kHz, 20 ms) of testdata/audio/synth/<name>.wav.
func packets(t *testing.T, name string) [][]byte {
	t.Helper()
	packetsMu.Lock()
	defer packetsMu.Unlock()
	if p, ok := packetsCache[name]; ok {
		return p
	}
	pcm, err := kcaudio.ReadWAV(filepath.Join(stack.root, "testdata", "audio", "synth", name+".wav"))
	require.NoError(t, err)
	p, err := kcaudio.PreEncode(pcm)
	require.NoError(t, err)
	packetsCache[name] = p
	return p
}

// recorder is a Sink that keeps every event and counts audio frames per generation.
type recorder struct {
	mu     sync.Mutex
	events []protocol.ServerEvent
	frames map[uint32]int
}

func newRecorder() *recorder { return &recorder{frames: map[uint32]int{}} }

func (r *recorder) OnEvent(e kotokiclient.Received[protocol.ServerEvent]) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e.Value)
}

func (r *recorder) OnAudio(f kotokiclient.AudioFrame) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.frames[f.Generation]++
}

// types returns the event types in arrival order.
func (r *recorder) types() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.events))
	for i, e := range r.events {
		out[i] = e.Type
	}
	return out
}

func (r *recorder) frameCount(gen uint32) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.frames[gen]
}

// of returns the payloads of the events of one type.
func (r *recorder) of(typ string) []protocol.ServerEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []protocol.ServerEvent
	for _, e := range r.events {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

// converse plays steps against the session on the realtime process.
func converse(t *testing.T, u *user, s kotokiclient.Session, steps []kotokiclient.Step, sink kotokiclient.Sink) (kotokiclient.Report, error) {
	t.Helper()
	return converseOn(t, stack.wsURL, u, s, steps, sink)
}

func converseOn(t *testing.T, wsURL string, u *user, s kotokiclient.Session, steps []kotokiclient.Step, sink kotokiclient.Sink) (kotokiclient.Report, error) {
	t.Helper()
	rep, err := kotokiclient.RunConversation(ctxT(t, scriptTimeout), u.Client, kotokiclient.SessionSpec{
		SessionID: s.ID, WSURL: wsURL, Origin: testOrigin,
		Client: &protocol.ClientInfo{Platform: "cli", AppVersion: "e2e"},
	}, steps, sink)
	if rec, ok := sink.(*recorder); ok && err != nil {
		t.Logf("conversation failed after events %v (frames per generation %v)", rec.types(), rec.frames)
	}
	return rep, err
}

// eventually is require.Eventually with the suite's bounds.
func eventually(t *testing.T, cond func() bool, msg string, args ...any) {
	t.Helper()
	require.Eventually(t, cond, eventuallyWait, eventuallyTick, append([]any{msg}, args...)...)
}

// turnRow is one session_turns row.
type turnRow struct {
	Seq                 int
	Status              *string
	UserText            *string
	AssistantText       *string
	AssistantSpokenText *string
	TurnEndToFirstAudio *int
	LLMFirstToken       *int
	TTSFirstAudio       *int
}

func turnRows(t *testing.T, sessionID uuid.UUID) []turnRow {
	t.Helper()
	rows, err := stack.pool.Query(t.Context(), `SELECT sequence_number, status, user_text, assistant_text, assistant_spoken_text,
		turn_end_to_first_audio_ms, llm_first_token_ms, tts_first_audio_ms
		FROM session_turns WHERE session_id = $1 ORDER BY sequence_number`, sessionID)
	require.NoError(t, err)
	defer rows.Close()
	var out []turnRow
	for rows.Next() {
		var r turnRow
		require.NoError(t, rows.Scan(&r.Seq, &r.Status, &r.UserText, &r.AssistantText, &r.AssistantSpokenText,
			&r.TurnEndToFirstAudio, &r.LLMFirstToken, &r.TTSFirstAudio))
		out = append(out, r)
	}
	require.NoError(t, rows.Err())
	return out
}

// ledgerRow is one balance_ledger row of a session.
type ledgerRow struct {
	Type      string
	Available int64
	Reserved  int64
}

func ledgerRows(t *testing.T, sessionID uuid.UUID) []ledgerRow {
	t.Helper()
	rows, err := stack.pool.Query(t.Context(), `SELECT entry_type, available_seconds_delta, reserved_seconds_delta
		FROM balance_ledger WHERE session_id = $1 ORDER BY account_version`, sessionID)
	require.NoError(t, err)
	defer rows.Close()
	var out []ledgerRow
	for rows.Next() {
		var r ledgerRow
		require.NoError(t, rows.Scan(&r.Type, &r.Available, &r.Reserved))
		out = append(out, r)
	}
	require.NoError(t, rows.Err())
	return out
}

// scrape reads /metrics of an ops listener into series -> value. A series is the metric name
// followed by its labels sorted by name, e.g. `m_bucket{kind="reply",le="0.5"}`.
func scrape(t *testing.T, opsAddr string) map[string]float64 {
	t.Helper()
	req, err := http.NewRequestWithContext(ctxT(t, 10*time.Second), http.MethodGet, "http://"+opsAddr+"/metrics", http.NoBody)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	return parseMetrics(t, resp.Body)
}

func parseMetrics(t *testing.T, r io.Reader) map[string]float64 {
	t.Helper()
	out := map[string]float64{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		i := strings.LastIndexByte(line, ' ')
		require.Positive(t, i, line)
		v, err := strconv.ParseFloat(line[i+1:], 64)
		require.NoError(t, err, line)
		out[canonicalSeries(line[:i])] = v
	}
	require.NoError(t, sc.Err())
	return out
}

// canonicalSeries sorts the labels of a series so lookups do not depend on exposition order.
func canonicalSeries(s string) string {
	open := strings.IndexByte(s, '{')
	if open < 0 {
		return s
	}
	return s[:open] + "{" + strings.Join(splitLabels(s[open+1:len(s)-1]), ",") + "}"
}

func splitLabels(body string) []string {
	var labels []string
	for body != "" {
		eq := strings.Index(body, `="`)
		end := eq + 2
		for end < len(body) && (body[end] != '"' || body[end-1] == '\\') {
			end++
		}
		labels = append(labels, body[:end+1])
		body = strings.TrimPrefix(body[min(end+1, len(body)):], ",")
	}
	sort.Strings(labels)
	return labels
}

// bucket is one cumulative histogram bucket.
type bucket struct {
	le    float64
	count float64
}

// histogram sums the `_bucket` series of name over every label set that contains all of want
// (e.g. `kind="reply"`), after subtracting the same series of before (nil = zero), and returns
// the buckets in ascending order with the total count.
func histogram(after, before map[string]float64, name string, want ...string) ([]bucket, float64) {
	byLE := map[float64]float64{}
	for series, v := range after {
		if !strings.HasPrefix(series, name+"_bucket{") || !hasLabels(series, want) {
			continue
		}
		le := labelValue(series, "le")
		f := math.Inf(1)
		if le != "+Inf" {
			f, _ = strconv.ParseFloat(le, 64)
		}
		byLE[f] += v - before[series]
	}
	out := make([]bucket, 0, len(byLE))
	for le, c := range byLE {
		out = append(out, bucket{le, c})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].le < out[j].le })
	if len(out) == 0 {
		return nil, 0
	}
	return out, out[len(out)-1].count
}

func hasLabels(series string, want []string) bool {
	for _, w := range want {
		if !strings.Contains(series, w) {
			return false
		}
	}
	return true
}

func labelValue(series, label string) string {
	i := strings.Index(series, label+`="`)
	if i < 0 {
		return ""
	}
	rest := series[i+len(label)+2:]
	return rest[:strings.IndexByte(rest, '"')]
}

// quantile estimates the q-quantile of cumulative buckets like PromQL's histogram_quantile: linear
// interpolation inside the bucket the rank falls in. It returns NaN without observations.
func quantile(b []bucket, q float64) float64 {
	if len(b) == 0 || b[len(b)-1].count == 0 {
		return math.NaN()
	}
	rank := q * b[len(b)-1].count
	prevLE, prevCount := 0.0, 0.0
	for _, bk := range b {
		if bk.count >= rank {
			if math.IsInf(bk.le, 1) {
				return prevLE
			}
			if bk.count == prevCount {
				return bk.le
			}
			return prevLE + (bk.le-prevLE)*(rank-prevCount)/(bk.count-prevCount)
		}
		prevLE, prevCount = bk.le, bk.count
	}
	return prevLE
}
