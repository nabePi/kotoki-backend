package session

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"regexp"
	"slices"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/balance"
	"github.com/nabePi/kotoki-backend/internal/content"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
)

// CreateSessionInput is POST /sessions (spec 5.2): the client chooses topic, scenario, character,
// duration and start mode; never a version, voice, style or provider (D-014).
type CreateSessionInput struct {
	UserID                uuid.UUID
	IdempotencyKey        string
	TopicID               uuid.UUID
	ScenarioID            uuid.UUID
	CharacterID           uuid.UUID
	DurationSeconds       int
	ConversationStartMode StartMode
}

// CreateSessionResult is the new session, or the existing one for a replayed request.
type CreateSessionResult struct {
	Session  Session
	Replayed bool
}

var idempotencyKeyRE = regexp.MustCompile(`^[A-Za-z0-9_-]{8,128}$`)

// HashCreateRequest is sha256 of the canonical JSON of the fields that decide what a session is:
// the idempotency key itself is not part of it (spec 4.6.1).
func HashCreateRequest(in CreateSessionInput) []byte {
	b, _ := json.Marshal(struct {
		TopicID               uuid.UUID `json:"topic_id"`
		ScenarioID            uuid.UUID `json:"scenario_id"`
		CharacterID           uuid.UUID `json:"character_id"`
		DurationSeconds       int       `json:"duration_seconds"`
		ConversationStartMode StartMode `json:"conversation_start_mode"`
	}{in.TopicID, in.ScenarioID, in.CharacterID, in.DurationSeconds, in.ConversationStartMode}) //nolint:errchkjson // fixed types cannot fail
	sum := sha256.Sum256(b)
	return sum[:]
}

func validationError(field, reason string) error {
	return errs.WithDetails(errs.New(errs.CodeValidationFailed, "invalid "+field),
		map[string]any{"fields": []map[string]any{{"field": field, "reason": reason}}})
}

func (s *Service) validateCreate(in CreateSessionInput) error {
	switch {
	case in.IdempotencyKey == "":
		return errs.New(errs.CodeIdempotencyKeyRequired, "Idempotency-Key header is required")
	case !idempotencyKeyRE.MatchString(in.IdempotencyKey):
		return errs.New(errs.CodeIdempotencyKeyInvalid, "Idempotency-Key must be 8–128 characters of A–Z a–z 0–9 _ -")
	case !slices.Contains(balance.AllowedDurations, int64(in.DurationSeconds)):
		return validationError("duration_seconds", "not_allowed")
	case in.ConversationStartMode != StartUserFirst && in.ConversationStartMode != StartAIFirst:
		return validationError("conversation_start_mode", "not_allowed")
	}
	return nil
}

// gate checks the account before any content work (spec 4.6.1 step 1).
func (s *Service) gate(ctx context.Context, userID uuid.UUID) error {
	info, err := s.d.Users.Gate(ctx, userID)
	if err != nil {
		return err
	}
	if info.Status != "ACTIVE" {
		return errs.New(errs.CodeAccountSuspended, "account is not active")
	}
	if info.TermsVersion != s.d.TermsVersion {
		return errs.WithDetails(errs.New(errs.CodeTermsNotAccepted, "current terms not accepted"), map[string]any{"current_version": s.d.TermsVersion})
	}
	return nil
}

// Create follows spec 4.6.1: gate, resolve, adaptive snapshot and hash outside any transaction,
// then one transaction that serialises the user on the account lock, replays or rejects by
// idempotency key, enforces one open session, reserves the minutes and writes SESSION_RESERVED.
func (s *Service) Create(ctx context.Context, in CreateSessionInput) (res CreateSessionResult, err error) {
	defer func() { s.countCreate(res, err) }()
	if err = s.validateCreate(in); err != nil {
		return CreateSessionResult{}, err
	}
	if err = s.gate(ctx, in.UserID); err != nil {
		return CreateSessionResult{}, err
	}
	rc, err := s.d.Resolver.Resolve(ctx, content.ResolveInput{UserID: in.UserID, TopicID: in.TopicID, ScenarioID: in.ScenarioID,
		CharacterID: in.CharacterID, ConversationStartMode: string(in.ConversationStartMode)})
	if err != nil {
		return CreateSessionResult{}, err
	}
	adaptive, err := s.d.Adaptive.Adaptive(ctx, AdaptiveRequest{UserID: in.UserID, Config: rc})
	if err != nil {
		return CreateSessionResult{}, errs.Wrap(err, errs.CodeServiceUnavailable, "adaptive difficulty unavailable")
	}
	snapshot, err := rc.ConfigSnapshot.MarshalJSONB()
	if err != nil {
		return CreateSessionResult{}, errs.Wrap(err, errs.CodeInternalError, "encode config snapshot failed")
	}
	hash := HashCreateRequest(in)

	out, err := s.createInTx(ctx, in, rc, snapshot, adaptive, hash)
	if errors.Is(err, ErrAlreadyOpen) || errors.Is(err, ErrDuplicateIdempotency) {
		// the unique index fired under a race the account lock did not cover: decide again, outside
		return s.decideAfterConflict(ctx, in, hash)
	}
	return out, err
}

func (s *Service) createInTx(ctx context.Context, in CreateSessionInput, rc content.ResolvedConfig, snapshot, adaptive []byte, hash []byte) (CreateSessionResult, error) {
	var out CreateSessionResult
	err := s.d.Store.WithinTx(ctx, func(ctx context.Context, tx Tx) error {
		out = CreateSessionResult{}
		acc, err := tx.Balance().LockAccountByUserID(ctx, in.UserID)
		if err != nil {
			return errs.Wrap(err, errs.CodeInternalError, "lock balance account failed")
		}
		repo := tx.Sessions()
		if existing, err := repo.FindByIdempotencyKey(ctx, in.UserID, in.IdempotencyKey); err == nil {
			out, err = replayOrReuse(existing, hash)
			return err
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}
		if open, err := repo.FindOpenByUser(ctx, in.UserID); err == nil {
			return alreadyOpen(open.ID)
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}

		now := s.d.Clock.Now()
		sess := Session{
			ID: s.d.IDs.New(), UserID: in.UserID, Status: StatusReserved, ConversationStartMode: in.ConversationStartMode, DurationSeconds: in.DurationSeconds,
			TopicID: rc.TopicID, ScenarioID: rc.ScenarioID, ScenarioVersionID: rc.ScenarioVersionID, CharacterID: rc.CharacterID, PromptVersionID: rc.PromptVersionID,
			ConfigSnapshot: snapshot, AdaptiveDifficulty: adaptive, ReservedSeconds: in.DurationSeconds,
			CreateIdempotencyKey: in.IdempotencyKey, CreateRequestHash: hash, CreatedAt: now, UpdatedAt: now, Version: 1,
		}
		if err := repo.Insert(ctx, sess); err != nil {
			return err
		}
		rid := s.d.IDs.New()
		if _, err := s.d.Balance.Reserve(ctx, tx, acc, balance.ReserveInput{ReservationID: rid, SessionID: sess.ID, Seconds: int64(in.DurationSeconds), At: now}); err != nil {
			return err
		}
		to := StatusReserved
		payload, _ := json.Marshal(map[string]any{"duration_seconds": in.DurationSeconds, "reservation_id": rid}) //nolint:errchkjson // fixed shape
		if err := tx.Events().Append(ctx, Event{ID: s.d.IDs.New(), SessionID: sess.ID, Type: EventReserved, Actor: ActorUser, ToStatus: &to, Payload: payload, CreatedAt: now}); err != nil {
			return err
		}
		out = CreateSessionResult{Session: sess}
		return nil
	})
	return out, err
}

func alreadyOpen(id uuid.UUID) error {
	return errs.WithDetails(errs.New(errs.CodeSessionAlreadyOpen, "user already has an open session"), map[string]any{"session_id": id})
}

func replayOrReuse(existing Session, hash []byte) (CreateSessionResult, error) {
	if !bytes.Equal(existing.CreateRequestHash, hash) {
		return CreateSessionResult{}, errs.New(errs.CodeIdempotencyKeyReused, "Idempotency-Key was used with a different request")
	}
	return CreateSessionResult{Session: existing, Replayed: true}, nil
}

// decideAfterConflict repeats checks 2 and 3 of spec 4.6.1 against the committed state.
func (s *Service) decideAfterConflict(ctx context.Context, in CreateSessionInput, hash []byte) (CreateSessionResult, error) {
	if existing, err := s.d.Reader.FindByIdempotencyKey(ctx, in.UserID, in.IdempotencyKey); err == nil {
		return replayOrReuse(existing, hash)
	} else if !errors.Is(err, ErrNotFound) {
		return CreateSessionResult{}, err
	}
	if open, err := s.d.Reader.FindOpenByUser(ctx, in.UserID); err == nil {
		return CreateSessionResult{}, alreadyOpen(open.ID)
	} else if !errors.Is(err, ErrNotFound) {
		return CreateSessionResult{}, err
	}
	return CreateSessionResult{}, errs.New(errs.CodeInternalError, "session create conflicted but no conflicting row was found")
}

// countCreate keeps kotoki_sessions_created_total honest: only a new session, a shortage of minutes
// and a server-side failure are counted; replays and other client errors are not.
func (s *Service) countCreate(res CreateSessionResult, err error) {
	switch {
	case err == nil && !res.Replayed:
		s.d.Metrics.Created("ok")
	case err == nil:
	case errs.CodeOf(err) == errs.CodeBalanceInsufficient:
		s.d.Metrics.Created("insufficient_balance")
	case isServerError(errs.CodeOf(err)):
		s.d.Metrics.Created("error")
	}
}

func isServerError(c errs.Code) bool {
	switch c {
	case "", errs.CodeInternalError, errs.CodeServiceUnavailable, errs.CodeContentMisconfigured:
		return true
	}
	return false
}
