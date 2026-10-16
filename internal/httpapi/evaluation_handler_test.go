package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/evaluation"
	"github.com/nabePi/kotoki-backend/internal/httpapi"
	"github.com/nabePi/kotoki-backend/internal/session"
)

// evalReader is the stored side of the evaluation endpoint in these tests.
type evalReader struct {
	row      *evaluation.Evaluation
	findings []evaluation.FindingRow
}

func (r *evalReader) GetBySession(context.Context, uuid.UUID) (evaluation.Evaluation, bool, error) {
	if r.row == nil {
		return evaluation.Evaluation{}, false, nil
	}
	return *r.row, true, nil
}

func (r *evalReader) ListFindings(context.Context, uuid.UUID) ([]evaluation.FindingRow, error) {
	return r.findings, nil
}

var (
	evalID      = uuid.MustParse("0192f1b5-3333-7aaa-8bbb-dddddddddddd")
	findingID   = uuid.MustParse("0192f1b5-4444-7aaa-8bbb-eeeeeeeeeeee")
	findingID2  = uuid.MustParse("0192f1b5-4444-7aaa-8bbb-eeeeeeeeeeef")
	findingTurn = uuid.MustParse("0192f1a0-5555-7aaa-8bbb-ffffffffffff")
	evalCreated = time.Date(2026, 10, 12, 6, 30, 16, 1_400_000, time.UTC)
	evalDone    = time.Date(2026, 10, 12, 6, 31, 2, 410_900_000, time.UTC)
)

func ip(n int) *int { return &n }

func sp(s string) *string { return &s }

func completedRow() *evaluation.Evaluation {
	overall := 52.399999999999 // float noise must not leak
	return &evaluation.Evaluation{
		ID: evalID, Status: evaluation.StatusCompleted, RubricVersion: "rubric-v1", OverallScore: &overall,
		Scores:                      evaluation.Scores{Grammar: ip(52), Vocabulary: ip(55), Fluency: ip(45), Naturalness: ip(50), TaskAchievement: ip(60)},
		ListeningResponseSignal:     ip(62),
		InteractionComplexitySignal: ip(38),
		DimensionConfidence:         map[string]float64{"GRAMMAR": 0.8},
		DimensionComments: map[string]string{
			"GRAMMAR": "Kalimat sederhanamu sudah jelas.", "VOCABULARY": "Kosakata cukup untuk topik harian.",
			"FLUENCY": "Masih banyak jeda.", "NATURALNESS": "Beberapa ungkapan terdengar kaku.", "TASK_ACHIEVEMENT": "Tujuan tercapai sebagian.",
		},
		SummaryFeedback: sp("Kamu bisa mempertahankan percakapan tentang pekerjaan."),
		Strengths:       []string{"Penggunaan です・ます konsisten."},
		FocusAreas:      []string{"Partikel で vs に", "Bentuk lampau kata sifat-i"},
		Recommendations: []evaluation.Recommendation{{Title: "Latihan partikel で", Detail: "Buat 5 kalimat.", ExampleJA: sp("図書館で勉強します。")}},
		Provider:        sp("OPENAI"), Model: sp("gpt-secret"), LLMCallCount: 1, InputTokens: ip(100), OutputTokens: ip(40),
		LastError: sp("internal detail"), AttemptCount: 2,
		CreatedAt: evalCreated, CompletedAt: &evalDone,
	}
}

func completedFindings() []evaluation.FindingRow {
	return []evaluation.FindingRow{
		{ID: findingID, Ordinal: 1, Finding: evaluation.Finding{
			Type: evaluation.FindingParticleError, Dimension: evaluation.DimGrammar, Severity: evaluation.SeverityMajor,
			TurnID: &findingTurn, TurnSeq: ip(1), Original: sp("会社に働いていました"), Corrected: sp("会社で働いていました"),
			PatternKey: sp("particle.de_ni_location"), Explanation: "Tempat aktivitas ditandai dengan で.", Confidence: 0.9, Source: "LLM"}},
		{ID: findingID2, Ordinal: 2, Finding: evaluation.Finding{
			Type: evaluation.FindingStrength, Dimension: evaluation.DimVocabulary, Severity: evaluation.SeverityInfo,
			Explanation: "Kosakata bervariasi.", Confidence: 0.8, Source: "LLM"}},
	}
}

// evalWorld seeds a funded user with a RESERVED session, then moves the session to status.
type evalWorld struct {
	e      *apiEnv
	authz  string
	id     uuid.UUID
	reader *evalReader
}

func newEvalWorld(t *testing.T, row *evaluation.Evaluation, findings []evaluation.FindingRow, status session.Status, started bool) *evalWorld {
	t.Helper()
	rd := &evalReader{row: row, findings: findings}
	e := newAPI(t, func(d *httpapi.Deps) { d.Evaluation = evaluation.NewQueryService(rd) })
	e.seedContent()
	authz := e.fundedUser(t, "hana@example.com", 1800)
	var created struct{ ID uuid.UUID }
	require.NoError(t, json.Unmarshal(e.createSession(t, authz, "key-0000001").Body.Bytes(), &created))
	w := &evalWorld{e: e, authz: authz, id: created.ID, reader: rd}
	w.setSession(func(s *session.Session) {
		s.Status = status
		if started {
			at := evalCreated.Add(-time.Minute)
			s.StartedAt = &at
		}
	})
	return w
}

func (w *evalWorld) setSession(mut func(*session.Session)) {
	s := w.e.sessions.Session(w.id)
	mut(&s)
	w.e.sessions.PutSession(s)
}

func (w *evalWorld) get(t *testing.T) (code int, body []byte, retryAfter string, m map[string]any) {
	t.Helper()
	rec := w.e.do(http.MethodGet, "/api/v1/sessions/"+w.id.String()+"/evaluation", "", "Authorization", w.authz)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &m), rec.Body.String())
	return rec.Code, rec.Body.Bytes(), rec.Header().Get("Retry-After"), m
}

var requestIDRE = regexp.MustCompile(`"request_id":"[^"]*"`)

// fixedRequestID pins the generated request id so that error bodies can be golden files.
func fixedRequestID(body []byte) []byte {
	return requestIDRE.ReplaceAll(body, []byte(`"request_id":"0192f1a0-1111-7000-8000-000000000001"`))
}

func TestEvaluationCompletedGolden(t *testing.T) {
	w := newEvalWorld(t, completedRow(), completedFindings(), session.StatusCompleted, true)
	code, body, retry, m := w.get(t)
	require.Equal(t, 200, code, string(body))
	require.Empty(t, retry)
	goldenFile(t, "", "evaluation_completed.json", body)

	require.Equal(t, 52.4, m["overall_score"])
	dims := m["dimensions"].([]any)
	require.Len(t, dims, 5)
	for i, want := range []string{"GRAMMAR", "VOCABULARY", "FLUENCY", "NATURALNESS", "TASK_ACHIEVEMENT"} {
		require.Equal(t, want, dims[i].(map[string]any)["dimension"])
	}
	require.Equal(t, "2026-10-12T06:30:16.001Z", m["created_at"])
	require.Equal(t, "2026-10-12T06:31:02.410Z", m["completed_at"])
	require.Nil(t, m["failure_message"])
	findings := m["findings"].([]any)
	require.Len(t, findings, 2)
	require.Equal(t, findingID.String(), findings[0].(map[string]any)["id"])
	require.Equal(t, findingTurn.String(), findings[0].(map[string]any)["turn_id"])
	require.Nil(t, findings[1].(map[string]any)["turn_id"])
	for _, banned := range []string{"provider", "model", "confidence", "pattern_key", "signals", "last_error", "input_tokens", "output_tokens", "listening_response_signal"} {
		require.NotContains(t, m, banned)
		for _, f := range findings {
			require.NotContains(t, f, banned)
		}
	}
	require.NotContains(t, string(body), "gpt-secret")
	require.NotContains(t, string(body), "internal detail")
}

func TestEvaluationInsufficientGolden(t *testing.T) {
	row := &evaluation.Evaluation{
		ID: evalID, Status: evaluation.StatusCompleted, InsufficientEvidence: true, RubricVersion: "rubric-v1",
		SummaryFeedback: sp(evaluation.InsufficientSummary), CreatedAt: evalCreated, CompletedAt: &evalDone,
	}
	w := newEvalWorld(t, row, nil, session.StatusCompleted, true)
	code, body, retry, m := w.get(t)
	require.Equal(t, 200, code, string(body))
	require.Empty(t, retry)
	goldenFile(t, "", "evaluation_insufficient.json", body)
	require.Equal(t, true, m["insufficient_evidence"])
	require.Nil(t, m["overall_score"])
	require.Equal(t, evaluation.InsufficientSummary, m["summary"])
	for _, d := range m["dimensions"].([]any) {
		require.Nil(t, d.(map[string]any)["score"])
	}
	for _, k := range []string{"strengths", "focus_areas", "recommendations", "findings"} {
		require.Equal(t, []any{}, m[k], k)
	}
}

func TestEvaluationPurgedGolden(t *testing.T) {
	row := completedRow()
	purged := evalDone.Add(180 * 24 * time.Hour)
	row.DetailsPurgedAt = &purged
	row.SummaryFeedback, row.Strengths, row.FocusAreas, row.Recommendations = nil, nil, nil, nil
	w := newEvalWorld(t, row, completedFindings(), session.StatusCompleted, true)
	code, body, _, m := w.get(t)
	require.Equal(t, 200, code, string(body))
	goldenFile(t, "", "evaluation_purged.json", body)
	require.Equal(t, true, m["details_purged"])
	require.Nil(t, m["summary"])
	require.Equal(t, 52.4, m["overall_score"], "scores survive the purge")
	require.Equal(t, float64(52), m["dimensions"].([]any)[0].(map[string]any)["score"])
	for _, d := range m["dimensions"].([]any) {
		require.Nil(t, d.(map[string]any)["comment"])
	}
	for _, k := range []string{"strengths", "focus_areas", "recommendations", "findings"} {
		require.Equal(t, []any{}, m[k], k)
	}
}

func TestEvaluationPurgedTranscriptHidesTurnIDs(t *testing.T) {
	w := newEvalWorld(t, completedRow(), completedFindings(), session.StatusCompleted, true)
	purged := evalDone
	w.setSession(func(s *session.Session) { s.TranscriptPurgedAt = &purged })
	_, _, _, m := w.get(t)
	f := m["findings"].([]any)[0].(map[string]any)
	require.Nil(t, f["turn_id"])
	require.Equal(t, float64(1), f["turn_seq"], "turn_seq stays")
	require.Equal(t, false, m["details_purged"])
}

func TestEvaluationRunningAndPendingRetryAfter(t *testing.T) {
	running := &evaluation.Evaluation{ID: evalID, Status: evaluation.StatusRunning, RubricVersion: "rubric-v1", CreatedAt: evalCreated}
	w := newEvalWorld(t, running, nil, session.StatusCompleted, true)
	code, body, retry, m := w.get(t)
	require.Equal(t, 200, code, string(body))
	require.Equal(t, "3", retry)
	goldenFile(t, "", "evaluation_running.json", body)
	require.Equal(t, "RUNNING", m["status"])
	require.Equal(t, evalID.String(), m["evaluation_id"])
	require.Nil(t, m["completed_at"])
	require.Nil(t, m["overall_score"])
	require.Equal(t, []any{}, m["findings"])

	w.reader.row.Status = evaluation.StatusPending
	_, _, retry, m = w.get(t)
	require.Equal(t, "3", retry)
	require.Equal(t, "PENDING", m["status"])

	// synthesized: no row yet, for a COMPLETED and for a FAILED session that had been ACTIVE
	for _, st := range []session.Status{session.StatusCompleted, session.StatusFailed} {
		w := newEvalWorld(t, nil, nil, st, true)
		code, body, retry, m := w.get(t)
		require.Equal(t, 200, code, string(body))
		require.Equal(t, "3", retry, st)
		require.Equal(t, "PENDING", m["status"])
		require.Nil(t, m["evaluation_id"])
		require.Nil(t, m["created_at"])
		require.Nil(t, m["completed_at"])
		if st == session.StatusCompleted {
			goldenFile(t, "", "evaluation_pending_synth.json", body)
		}
	}
}

func TestEvaluationFailedGolden(t *testing.T) {
	row := &evaluation.Evaluation{
		ID: evalID, Status: evaluation.StatusFailed, RubricVersion: "rubric-v1", CreatedAt: evalCreated,
		FailureCode: failureCode(evaluation.FailInvalidModelOutput), LastError: sp("boom"), AttemptCount: 5,
	}
	w := newEvalWorld(t, row, nil, session.StatusCompleted, true)
	code, body, retry, m := w.get(t)
	require.Equal(t, 200, code, string(body))
	require.Empty(t, retry)
	goldenFile(t, "", "evaluation_failed.json", body)
	require.Equal(t, "FAILED", m["status"])
	require.Equal(t, "Evaluasi untuk sesi ini gagal diproses. Tim kami sudah diberi tahu.", m["failure_message"])
	require.Nil(t, m["overall_score"])
	require.NotContains(t, string(body), "INVALID_MODEL_OUTPUT")
	require.NotContains(t, string(body), "boom")
}

func failureCode(c evaluation.FailureCode) *evaluation.FailureCode { return &c }

func TestEvaluationNotEnded409(t *testing.T) {
	for _, st := range []session.Status{session.StatusReserved, session.StatusStarting, session.StatusActive, session.StatusEnding} {
		w := newEvalWorld(t, completedRow(), nil, st, st != session.StatusReserved)
		code, body, retry, m := w.get(t)
		require.Equal(t, 409, code, string(body))
		require.Empty(t, retry)
		e := m["error"].(map[string]any)
		require.Equal(t, "SESSION_NOT_ENDED", e["code"])
		require.Equal(t, "Evaluasi baru tersedia setelah sesi selesai.", e["message"])
		require.Equal(t, map[string]any{"session_status": string(st)}, e["details"])
		if st == session.StatusReserved {
			goldenFile(t, "", "evaluation_not_ended.json", fixedRequestID(body))
		}
	}
}

func TestEvaluationNotAvailable404(t *testing.T) {
	for _, st := range []session.Status{session.StatusCancelled, session.StatusAbandoned, session.StatusFailed} {
		w := newEvalWorld(t, completedRow(), nil, st, false)
		code, body, _, m := w.get(t)
		require.Equal(t, 404, code, string(body))
		require.Equal(t, "EVALUATION_NOT_AVAILABLE", m["error"].(map[string]any)["code"], st)
		if st == session.StatusCancelled {
			goldenFile(t, "", "evaluation_not_available.json", fixedRequestID(body))
		}
	}
}

func TestEvaluationOtherUsersAndBadIDs(t *testing.T) {
	w := newEvalWorld(t, completedRow(), completedFindings(), session.StatusCompleted, true)
	other := w.e.fundedUser(t, "other@example.com", 600)

	rec := w.e.do(http.MethodGet, "/api/v1/sessions/"+w.id.String()+"/evaluation", "", "Authorization", other)
	require.Equal(t, 404, rec.Code)
	require.Equal(t, "SESSION_NOT_FOUND", errorBody(t, rec)["code"])
	require.NotContains(t, rec.Body.String(), evalID.String())

	for _, p := range []string{uuid.NewString(), "not-a-uuid"} {
		rec := w.e.do(http.MethodGet, "/api/v1/sessions/"+p+"/evaluation", "", "Authorization", w.authz)
		require.Equal(t, 404, rec.Code, p)
		require.Equal(t, "SESSION_NOT_FOUND", errorBody(t, rec)["code"], p)
	}

	rec = w.e.do(http.MethodGet, "/api/v1/sessions/"+w.id.String()+"/evaluation", "")
	require.Equal(t, 401, rec.Code)
	require.Equal(t, "AUTH_REQUIRED", errorBody(t, rec)["code"])
}

func TestEvaluationRouteAbsentWithoutService(t *testing.T) {
	e := newAPI(t, func(d *httpapi.Deps) { d.Evaluation = nil })
	rec := e.do(http.MethodGet, "/api/v1/sessions/"+uuid.NewString()+"/evaluation", "")
	require.Equal(t, 404, rec.Code)
	require.Equal(t, "NOT_FOUND", errorBody(t, rec)["code"])
}

func TestEvaluationOverallScoreRoundedToTwoDecimals(t *testing.T) {
	row := completedRow()
	noisy := 52.43000000001
	row.OverallScore = &noisy
	w := newEvalWorld(t, row, nil, session.StatusCompleted, true)
	_, body, _, m := w.get(t)
	require.Equal(t, 52.43, m["overall_score"])
	require.Contains(t, string(body), `"overall_score":52.43,`)
}
