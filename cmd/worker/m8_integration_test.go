//go:build integration

package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/events"
	"github.com/nabePi/kotoki-backend/internal/postgres"
	"github.com/nabePi/kotoki-backend/internal/testutil"
)

func m8Turns() []testutil.TurnSeed {
	return []testutil.TurnSeed{
		{UserText: "私はバックエンドエンジニアとして働いています。", AssistantText: "そうですか。", UserSeconds: 25},
		{UserText: "毎朝コーヒーを飲んでから出かけます。", AssistantText: "いいですね。", UserSeconds: 25},
		{UserText: "助詞の使い方がまだ少し難しいです。", AssistantText: "大丈夫ですよ。", UserSeconds: 25},
		{UserText: "週末は家族と公園に行きたいです。", AssistantText: "素敵ですね。", UserSeconds: 25},
	}
}

func enqueueSessionCompleted(t *testing.T, store *postgres.Store, fs testutil.FinishedSession) {
	t.Helper()
	require.NoError(t, store.WithinTx(context.Background(), func(ctx context.Context, tx *postgres.Tx) error {
		return tx.Outbox().Enqueue(ctx, events.NewSessionCompleted(fs.Envelope.EventID, fs.Payload.EndedAt, events.ProducerRealtime, fs.UserID, fs.Payload))
	}))
}

// The whole worker as deployed with M8 on a real broker: a session.completed in the outbox ends as
// memories (memory consumer), and the evaluation.completed of the evaluation consumer ends as a
// skill state (learner consumer); both consumers leave their processed_events row.
func TestWorker_MemoryLearner_EndToEndOnBroker(t *testing.T) {
	_, pool, _ := workerEnv(t, map[string]string{"PROVIDER_MODE": "fake"})
	fs := testutil.SeedFinishedSession(t, pool, testutil.FinishedSessionOpts{Turns: m8Turns(), ActiveSeconds: 120})
	enqueueSessionCompleted(t, postgres.NewStore(pool), fs)
	runWorker(t, "outbox,evaluation,memory,learner")

	q := func(sql string, args ...any) int {
		var n int
		_ = pool.QueryRow(context.Background(), sql, args...).Scan(&n)
		return n
	}
	require.Eventually(t, func() bool {
		return q(`SELECT count(*) FROM memories WHERE user_id = $1 AND status = 'ACTIVE'`, fs.UserID) > 0 &&
			q(`SELECT count(*) FROM processed_events WHERE consumer = 'memory.session_completed' AND event_id = $1`, fs.Envelope.EventID) == 1
	}, 30*time.Second, 100*time.Millisecond, "memories written and memory.session_completed marked processed")
	assert.Equal(t, 1, q(`SELECT count(*) FROM memories WHERE user_id = $1 AND category = 'EPISODIC'`, fs.UserID))
	assert.Equal(t, 0, q(`SELECT count(*) FROM memories WHERE user_id = $1 AND source_session_id <> $2`, fs.UserID, fs.ID))

	require.Eventually(t, func() bool {
		return q(`SELECT count(*) FROM learner_skill_states WHERE user_id = $1 AND evidence_count > 0`, fs.UserID) > 0 &&
			q(`SELECT count(*) FROM processed_events WHERE consumer = 'learner.evaluation_completed'`) == 1
	}, 30*time.Second, 100*time.Millisecond, "evaluation.completed updated a skill state and learner.evaluation_completed is marked processed")
	assert.Positive(t, q(`SELECT count(*) FROM proficiency_estimates WHERE user_id = $1`, fs.UserID))
	assert.Equal(t, 1, q(`SELECT count(*) FROM learner_profiles WHERE user_id = $1`, fs.UserID))
}

// The memver of a user moves once the extraction committed (Redis), so a running session drops its
// stale memory context.
func TestWorker_Memory_BumpsMemverInRedis(t *testing.T) {
	env, pool, _ := workerEnv(t, map[string]string{"PROVIDER_MODE": "fake"})
	fs := testutil.SeedFinishedSession(t, pool, testutil.FinishedSessionOpts{Turns: m8Turns(), ActiveSeconds: 120})
	enqueueSessionCompleted(t, postgres.NewStore(pool), fs)
	runWorker(t, "outbox,memory")

	opts, err := redis.ParseURL(env["REDIS_URL"])
	require.NoError(t, err)
	rdb := redis.NewClient(opts)
	t.Cleanup(func() { _ = rdb.Close() })
	require.Eventually(t, func() bool {
		v, err := rdb.Get(context.Background(), "kotoki:user:"+fs.UserID.String()+":memver").Int64()
		return err == nil && v == 1
	}, 30*time.Second, 100*time.Millisecond)
}

// The binary itself (no cgo, as deployed): boots, turns ready, and exits 0 on SIGTERM.
func TestWorkerBinary_SmokeSigterm(t *testing.T) {
	env, _, _ := workerEnv(t, map[string]string{"PROVIDER_MODE": "fake"})
	bin := filepath.Join(t.TempDir(), "kotoki-worker")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	out, err := build.CombinedOutput()
	require.NoError(t, err, string(out))

	var logs bytes.Buffer
	cmd := exec.Command(bin, "--handlers=outbox,memory,learner")
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Stdout, cmd.Stderr = &logs, &logs
	require.NoError(t, cmd.Start())
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			<-exited
		}
	})

	require.Eventually(t, func() bool { c, _ := status(env["WORKER_HTTP_ADDR"], "/readyz"); return c == 200 },
		30*time.Second, 100*time.Millisecond, "/readyz 200; logs:\n%s", &logs)
	require.NoError(t, cmd.Process.Signal(syscall.SIGTERM))
	select {
	case err := <-exited:
		require.NoError(t, err, "exit status 0 on SIGTERM; logs:\n%s", &logs)
	case <-time.After(20 * time.Second):
		t.Fatalf("worker did not exit after SIGTERM; logs:\n%s", &logs)
	}
}
