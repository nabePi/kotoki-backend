//go:build integration

package main

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/events"
	"github.com/nabePi/kotoki-backend/internal/postgres"
	"github.com/nabePi/kotoki-backend/internal/testutil"
)

// runWorker starts the real worker (runWith, server mode) and returns a stop function that cancels
// it and waits for a clean exit.
func runWorker(t *testing.T, handlers string) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runWith(ctx, []string{"--handlers=" + handlers}, io.Discard, io.Discard) }()
	var stopped bool
	stop = func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(15 * time.Second):
			t.Fatal("worker did not stop after cancel")
		}
	}
	t.Cleanup(stop)
	return stop
}

// --handlers=evaluation,summary builds both consumers (fake providers) and the worker turns ready.
func TestWorker_RunEvaluationFlags(t *testing.T) {
	env, _, _ := workerEnv(t, map[string]string{"PROVIDER_MODE": "fake"})
	runWorker(t, "evaluation,summary")
	require.Eventually(t, func() bool { c, _ := status(env["WORKER_HTTP_ADDR"], "/readyz"); return c == 200 },
		20*time.Second, 50*time.Millisecond, "/readyz 200 with evaluation and summary consumers")
}

// PROVIDER_MODE=real is accepted by the consumers that call providers, but a configuration that cannot
// start is refused before any connection, naming the variable.
func TestWorker_RealProviderModeWithoutKeysRefusedForConsumers(t *testing.T) {
	// RabbitMQ is unreachable: the provider check comes before any connection, so the key is the error.
	workerEnv(t, map[string]string{"PROVIDER_MODE": "real", "OPENAI_API_KEY": "", "DEEPSEEK_API_KEY": "", "RABBITMQ_URL": "amqp://u:p@127.0.0.1:1/"})
	t.Setenv("OPENAI_API_KEY", "")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := runWith(ctx, []string{"--handlers=evaluation"}, io.Discard, io.Discard)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "OPENAI_API_KEY")
}

// A real configuration with keys (fake values: nothing calls a vendor) builds the router from the
// vendor adapters, and the evaluation and summary consumers turn ready. ElevenLabs/Cartesia keys are
// not needed: the worker only calls LLM/EVALUATION and EMBEDDING.
func TestWorker_RunRealModeWithKeys(t *testing.T) {
	env, _, _ := workerEnv(t, map[string]string{
		"PROVIDER_MODE": "real", "OPENAI_API_KEY": "fake-openai", "ELEVENLABS_API_KEY": "", "CARTESIA_API_KEY": "",
	})
	t.Setenv("ELEVENLABS_API_KEY", "")
	t.Setenv("CARTESIA_API_KEY", "")
	runWorker(t, "evaluation,summary")
	require.Eventually(t, func() bool { c, _ := status(env["WORKER_HTTP_ADDR"], "/readyz"); return c == 200 },
		20*time.Second, 50*time.Millisecond, "/readyz 200 in real mode with keys")
}

// A handler set without provider consumers never validates the provider configuration.
func TestWorker_RealProviderModeIgnoredWithoutConsumers(t *testing.T) {
	env, _, _ := workerEnv(t, map[string]string{"PROVIDER_MODE": "real", "OPENAI_API_KEY": ""})
	t.Setenv("OPENAI_API_KEY", "")
	runWorker(t, "outbox")
	require.Eventually(t, func() bool { c, _ := status(env["WORKER_HTTP_ADDR"], "/readyz"); return c == 200 },
		20*time.Second, 50*time.Millisecond)
}

// The whole worker, as deployed: outbox publisher + evaluation + summary consumers in one process.
// A session.completed in the outbox ends as a COMPLETED evaluation and a summary.
func TestWorker_PipelineThroughRun(t *testing.T) {
	_, pool, _ := workerEnv(t, map[string]string{"PROVIDER_MODE": "fake"})
	fs := testutil.SeedFinishedSession(t, pool, testutil.FinishedSessionOpts{
		Turns: []testutil.TurnSeed{
			{UserText: "昨日は友達と一緒に映画を見ました。", AssistantText: "そうですか。", UserSeconds: 25},
			{UserText: "今日は会社で会議がありました。", AssistantText: "大変でしたね。", UserSeconds: 25},
			{UserText: "週末は家族と公園に行きたいです。", AssistantText: "いいですね。", UserSeconds: 25},
			{UserText: "毎朝コーヒーを飲んでから出かけます。", AssistantText: "私もです。", UserSeconds: 25},
		},
		ActiveSeconds: 120,
	})
	store := postgres.NewStore(pool)
	require.NoError(t, store.WithinTx(context.Background(), func(ctx context.Context, tx *postgres.Tx) error {
		return tx.Outbox().Enqueue(ctx, events.NewSessionCompleted(fs.Envelope.EventID, fs.Payload.EndedAt, events.ProducerRealtime, fs.UserID, fs.Payload))
	}))
	runWorker(t, "outbox,evaluation,summary")

	require.Eventually(t, func() bool {
		var evals, sums, completed int
		_ = pool.QueryRow(context.Background(), `SELECT count(*) FROM evaluations WHERE session_id = $1 AND status = 'COMPLETED'`, fs.ID).Scan(&evals)
		_ = pool.QueryRow(context.Background(), `SELECT count(*) FROM sessions WHERE id = $1 AND summary_text IS NOT NULL`, fs.ID).Scan(&sums)
		_ = pool.QueryRow(context.Background(), `SELECT count(*) FROM event_outbox WHERE event_type = 'evaluation.completed' AND status = 'PUBLISHED'`).Scan(&completed)
		return evals == 1 && sums == 1 && completed == 1
	}, 30*time.Second, 100*time.Millisecond, "evaluation COMPLETED, summary_text set, evaluation.completed published")
}
