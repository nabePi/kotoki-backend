package postgres

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nabePi/kotoki-backend/internal/postgres/sqlcgen"
	"github.com/nabePi/kotoki-backend/internal/session"
)

type turnRepo struct{ q *sqlcgen.Queries }

var _ session.TurnRepository = (*turnRepo)(nil)

// NewTurnRepo returns a session.TurnRepository over the pool, for reads outside a transaction.
func NewTurnRepo(pool *pgxpool.Pool) session.TurnRepository { return &turnRepo{q: sqlcgen.New(pool)} }

// Turns returns the transaction's turn repository.
func (t *Tx) Turns() session.TurnRepository { return &turnRepo{q: t.Queries()} }

// partitionSlack is how far before the session's starting_at a turn's created_at may lie (clock
// skew between pods), used for partition pruning (spec 5.3.2).
const partitionSlack = time.Minute

func toTurn(r sqlcgen.SessionTurn) session.Turn {
	t := session.Turn{
		ID: r.ID, SessionID: r.SessionID, Sequence: int(r.SequenceNumber), UserText: fromText(r.UserText),
		UserStartedAt: fromTszPtr(r.UserStartedAt), UserEndedAt: fromTszPtr(r.UserEndedAt), AssistantText: fromText(r.AssistantText),
		AssistantSpokenText: fromText(r.AssistantSpokenText), Generation: int(r.Generation), STTProvider: fromText(r.SttProvider),
		LLMProvider: fromText(r.LlmProvider), LLMModel: fromText(r.LlmModel), TTSProvider: fromText(r.TtsProvider), TTSModel: fromText(r.TtsModel),
		TTSVoiceID: fromText(r.TtsVoiceID), UsedFallback: r.UsedFallback, TurnEndToFirstAudioMS: fromInt4(r.TurnEndToFirstAudioMs),
		STTFinalMS: fromInt4(r.SttFinalMs), LLMFirstTokenMS: fromInt4(r.LlmFirstTokenMs), TTSFirstAudioMS: fromInt4(r.TtsFirstAudioMs),
		InputTokens: fromInt4(r.InputTokens), OutputTokens: fromInt4(r.OutputTokens), TTSCharacters: fromInt4(r.TtsCharacters),
		STTAudioMS: fromInt4(r.SttAudioMs), ErrorCode: fromText(r.ErrorCode), CreatedAt: fromTsz(r.CreatedAt), CompletedAt: fromTszPtr(r.CompletedAt),
	}
	if r.Status.Valid {
		st := session.TurnStatus(r.Status.String)
		t.Status = &st
	}
	return t
}

func fromInt4(v pgtype.Int4) *int {
	if !v.Valid {
		return nil
	}
	n := int(v.Int32)
	return &n
}

func turnList(rows []sqlcgen.SessionTurn) []session.Turn {
	out := make([]session.Turn, len(rows))
	for i, r := range rows {
		out[i] = toTurn(r)
	}
	return out
}

// fenced turns a "no row" result of a fenced write into ErrFenceLost.
func fenced(n int64, err error, what string) error {
	if err != nil {
		return internalErr(err, what+" failed")
	}
	if n == 0 {
		return session.ErrFenceLost
	}
	return nil
}

func (r *turnRepo) Insert(ctx context.Context, fence int64, t session.TurnStart) error {
	n, err := r.q.InsertTurnFenced(ctx, sqlcgen.InsertTurnFencedParams{ID: t.ID, SessionID: t.SessionID, SequenceNumber: int32(t.Sequence), //nolint:gosec // small counters
		Generation: int32(t.Generation), CreatedAt: tsz(t.CreatedAt), Fence: fence}) //nolint:gosec // small counters
	return fenced(n, err, "insert turn")
}

func (r *turnRepo) UpdateUser(ctx context.Context, fence int64, turnID uuid.UUID, u session.TurnUser) error {
	n, err := r.q.UpdateTurnUserFenced(ctx, sqlcgen.UpdateTurnUserFencedParams{
		UserText: pgtype.Text{String: u.Text, Valid: true}, UserStartedAt: tszPtr(u.StartedAt), UserEndedAt: tszPtr(u.EndedAt),
		SttProvider: textPtr(u.STTProvider), SttFinalMs: optInt4(u.STTFinalMS), SttAudioMs: optInt4(u.STTAudioMS),
		ID: turnID, CreatedAt: tsz(u.CreatedAt), Fence: fence,
	})
	return fenced(n, err, "update turn user side")
}

func (r *turnRepo) Finalize(ctx context.Context, fence int64, turnID uuid.UUID, f session.TurnFinal) error {
	n, err := r.q.FinalizeTurnFenced(ctx, sqlcgen.FinalizeTurnFencedParams{
		Status: pgtype.Text{String: string(f.Status), Valid: true}, AssistantText: textPtr(f.AssistantText), AssistantSpokenText: textPtr(f.AssistantSpokenText),
		LlmProvider: textPtr(f.LLMProvider), LlmModel: textPtr(f.LLMModel), TtsProvider: textPtr(f.TTSProvider), TtsModel: textPtr(f.TTSModel),
		TtsVoiceID: textPtr(f.TTSVoiceID), UsedFallback: f.UsedFallback, TurnEndToFirstAudioMs: optInt4(f.TurnEndToFirstAudioMS),
		LlmFirstTokenMs: optInt4(f.LLMFirstTokenMS), TtsFirstAudioMs: optInt4(f.TTSFirstAudioMS), InputTokens: optInt4(f.InputTokens),
		OutputTokens: optInt4(f.OutputTokens), TtsCharacters: optInt4(f.TTSCharacters), ErrorCode: textPtr(f.ErrorCode),
		CompletedAt: tsz(f.CompletedAt), Generation: optInt4(f.Generation), ID: turnID, CreatedAt: tsz(f.CreatedAt), Fence: fence,
	})
	return fenced(n, err, "finalize turn")
}

// FailInProgress marks every turn still in progress FAILED (takeover). With nothing to fail it
// checks the fence separately, so a stale runtime learns it lost the session either way.
func (r *turnRepo) FailInProgress(ctx context.Context, fence int64, sessionID uuid.UUID, partial *session.TurnFinal) (int, error) {
	arg := sqlcgen.FailInProgressTurnsFencedParams{SessionID: sessionID, Fence: fence}
	if partial != nil {
		arg.ErrorCode, arg.AssistantText, arg.AssistantSpokenText = textPtr(partial.ErrorCode), textPtr(partial.AssistantText), textPtr(partial.AssistantSpokenText)
	}
	n, err := r.q.FailInProgressTurnsFenced(ctx, arg)
	if err != nil {
		return 0, internalErr(err, "fail in-progress turns failed")
	}
	if n == 0 {
		cur, err := r.q.GetSessionByID(ctx, sessionID)
		if err != nil {
			return 0, sessErr(err, "read session")
		}
		if cur.RuntimeFence != fence || !session.Status(cur.Status).Open() || cur.Status == string(session.StatusReserved) {
			return 0, session.ErrFenceLost
		}
	}
	return int(n), nil
}

func (r *turnRepo) ListRecent(ctx context.Context, sessionID uuid.UUID, limit int) ([]session.TurnRecord, error) {
	rows, err := r.q.ListRecentTurns(ctx, sqlcgen.ListRecentTurnsParams{SessionID: sessionID, RowLimit: limit32(limit)})
	if err != nil {
		return nil, internalErr(err, "list recent turns failed")
	}
	return turnList(rows), nil
}

func (r *turnRepo) MaxSeqAndGeneration(ctx context.Context, sessionID uuid.UUID) (int, int, error) {
	row, err := r.q.MaxTurnSequenceAndGeneration(ctx, sessionID)
	if err != nil {
		return 0, 0, internalErr(err, "read max turn sequence failed")
	}
	return int(row.MaxSeq), int(row.MaxGen), nil
}

func (r *turnRepo) FailDangling(ctx context.Context, sessionID uuid.UUID, startingAt, at time.Time) (int, error) {
	n, err := r.q.FailDanglingTurns(ctx, sqlcgen.FailDanglingTurnsParams{At: tsz(at), SessionID: sessionID, Since: tsz(startingAt.Add(-partitionSlack))})
	if err != nil {
		return 0, internalErr(err, "fail dangling turns failed")
	}
	return int(n), nil
}

func (r *turnRepo) Stats(ctx context.Context, sessionID uuid.UUID, startingAt time.Time) (int, int, bool, error) {
	row, err := r.q.TurnStats(ctx, sqlcgen.TurnStatsParams{SessionID: sessionID, Since: tsz(startingAt.Add(-partitionSlack))})
	if err != nil {
		return 0, 0, false, internalErr(err, "read turn stats failed")
	}
	return int(row.TurnCount), int(row.UserTurnCount), row.HasAudio, nil
}

func (r *turnRepo) ListForUser(ctx context.Context, sessionID uuid.UUID, startingAt time.Time, endedAt *time.Time, afterSeq *int, limit int) ([]session.TurnRecord, error) {
	arg := sqlcgen.ListTurnsForUserParams{SessionID: sessionID, Since: tsz(startingAt.Add(-partitionSlack)), AfterSeq: optInt4(afterSeq), RowLimit: limit32(limit)}
	if endedAt != nil {
		arg.Until = tsz(endedAt.Add(partitionSlack))
	}
	rows, err := r.q.ListTurnsForUser(ctx, arg)
	if err != nil {
		return nil, internalErr(err, "list turns failed")
	}
	return turnList(rows), nil
}
