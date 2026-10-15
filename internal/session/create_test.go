package session_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/balance"
	"github.com/nabePi/kotoki-backend/internal/content"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
	"github.com/nabePi/kotoki-backend/internal/session"
	"github.com/nabePi/kotoki-backend/internal/testutil/fakes"
)

var t0 = time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)

const terms = "2026-10-01"

type recMetrics struct {
	mu      sync.Mutex
	created []string
	ended   [][3]string
}

func (m *recMetrics) Created(r string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.created = append(m.created, r)
}

func (m *recMetrics) Ended(service, reason, status string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ended = append(m.ended, [3]string{service, reason, status})
}

type env struct {
	svc      *session.Service
	store    *fakes.SessionStore
	bal      *fakes.BalanceStore
	users    *fakes.UserGate
	adaptive *fakes.Adaptive
	opening  *fakes.OpeningSource
	control  *fakes.ControlPublisher
	logs     *bytes.Buffer
	repo     *fakes.ContentRepo
	clk      *clock.Fake
	metrics  *recMetrics
	ids      idgen.Generator

	topic     content.Topic
	scenario  content.Scenario
	sv        content.ScenarioVersion
	character content.Character
	prompt    content.PromptVersion
	userID    uuid.UUID
}

func newEnv(t *testing.T, availableSeconds int64) *env {
	t.Helper()
	ids := idgen.NewSequence(t)
	e := &env{ids: ids, clk: clock.NewFake(t0), metrics: &recMetrics{}, repo: fakes.NewContentRepo()}
	e.bal = fakes.NewBalanceStore(fakes.WithBalanceIDs(ids))
	e.store = fakes.NewSessionStore(e.bal)
	e.users = fakes.NewUserGate(session.UserGateInfo{Status: "ACTIVE", TermsVersion: terms})
	e.adaptive = fakes.NewAdaptive("")
	e.opening = &fakes.OpeningSource{}
	e.control = &fakes.ControlPublisher{}
	e.logs = &bytes.Buffer{}
	e.userID = uuid.Must(uuid.NewV7())

	e.topic = content.Topic{ID: uuid.New(), Code: "JOB_INTERVIEW", Name: "Job", Status: content.StatusActive}
	e.scenario = content.Scenario{ID: uuid.New(), TopicID: e.topic.ID, Code: "BACKEND", Name: "Backend", Status: content.StatusActive}
	e.sv = content.ScenarioVersion{ID: uuid.New(), ScenarioID: e.scenario.ID, Version: 1, Status: content.VersionPublished, Formality: content.FormalityKeigo, DifficultyLevel: 4}
	e.character = content.Character{ID: uuid.New(), Code: "HARUKA", Status: content.StatusActive, Name: "Haruka", Persona: strings.Repeat("A warm guide. ", 5),
		ProviderName: "ELEVENLABS", ModelName: "m", VoiceID: "v", Voice: content.Voice{Gender: content.GenderFemale, Accent: content.AccentKanto, SpeakingRate: 1,
			Fallback: &content.TTSBinding{ProviderName: "CARTESIA", ModelName: "sonic", VoiceID: "fb"}}}
	e.prompt = content.PromptVersion{ID: uuid.New(), Key: content.PromptConversation, Version: 1, Status: content.VersionPublished, SystemTemplate: "x"}
	e.putContent()

	if availableSeconds > 0 {
		require.NoError(t, e.bal.WithinTx(context.Background(), func(ctx context.Context, tx balance.Tx) error {
			return balance.OpenAccount(ctx, tx, ids, e.userID, availableSeconds, t0)
		}))
	}
	resolver := content.NewResolver(e.repo, content.ResolverConfig{TTSMode: "fake", LLMConfigured: true}, nil)
	bal := balance.NewService(e.bal, e.clk, ids)
	e.svc = session.NewService(session.Deps{Store: e.store, Reader: e.store.Reader(), Balance: bal, Resolver: resolver, Adaptive: e.adaptive, Users: e.users,
		OpeningPrompts: e.opening, Control: e.control, Log: slog.New(slog.NewJSONHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Clock: e.clk, IDs: ids, TermsVersion: terms, Metrics: e.metrics, ServiceName: "api"})
	return e
}

func (e *env) putContent() {
	e.repo.PutTopic(e.topic)
	e.repo.PutScenario(e.scenario)
	e.repo.PutScenarioVersion(e.sv)
	e.repo.PutCharacter(e.character)
	e.repo.PutPromptVersion(e.prompt)
}

func (e *env) input(key string) session.CreateSessionInput {
	return session.CreateSessionInput{UserID: e.userID, IdempotencyKey: key, TopicID: e.topic.ID, ScenarioID: e.scenario.ID, CharacterID: e.character.ID,
		DurationSeconds: 900, ConversationStartMode: session.StartUserFirst}
}

func code(err error) errs.Code { return errs.CodeOf(err) }

func TestCreateHappyPath(t *testing.T) {
	e := newEnv(t, 1800)
	res, err := e.svc.Create(context.Background(), e.input("key-0000001"))
	require.NoError(t, err)
	require.False(t, res.Replayed)
	s := res.Session
	require.Equal(t, session.StatusReserved, s.Status)
	require.Equal(t, 900, s.ReservedSeconds)
	require.Equal(t, e.sv.ID, s.ScenarioVersionID)
	require.Equal(t, e.prompt.ID, s.PromptVersionID)
	require.Equal(t, e.character.ID, s.CharacterID)
	require.Nil(t, s.OpeningPromptVersionID)
	require.JSONEq(t, `{"formality":"POLITE"}`, string(s.AdaptiveDifficulty), "copied from the adaptive provider")

	acc := e.bal.Account(e.userID)
	require.Equal(t, int64(900), acc.AvailableSeconds)
	require.Equal(t, int64(900), acc.ReservedSeconds)
	res1 := e.bal.Reservation(s.ID)
	require.Equal(t, balance.ReservationHeld, res1.Status)
	require.Equal(t, int64(900), res1.ReservedSeconds)
	require.Equal(t, t0.Add(5*time.Minute), res1.StartDeadlineAt)

	evs := e.store.Events(s.ID)
	require.Len(t, evs, 1)
	require.Equal(t, session.EventReserved, evs[0].Type)
	require.Equal(t, session.ActorUser, evs[0].Actor)
	require.Nil(t, evs[0].FromStatus)
	require.Equal(t, session.StatusReserved, *evs[0].ToStatus)
	require.JSONEq(t, `{"duration_seconds":900,"reservation_id":"`+res1.ID.String()+`"}`, string(evs[0].Payload))

	reqs := e.adaptive.Requests()
	require.Len(t, reqs, 1)
	require.Equal(t, e.userID, reqs[0].UserID)
	require.Equal(t, content.FormalityKeigo, reqs[0].Config.ConfigSnapshot.Formality, "adaptive reads the base formality")
	require.Equal(t, content.AccentKanto, reqs[0].Config.ConfigSnapshot.Character.Voice.Accent)
	require.Equal(t, []string{"ok"}, e.metrics.created)
}

func TestCreateReplaySameKeySamePayload(t *testing.T) {
	e := newEnv(t, 1800)
	first, err := e.svc.Create(context.Background(), e.input("key-0000001"))
	require.NoError(t, err)
	again, err := e.svc.Create(context.Background(), e.input("key-0000001"))
	require.NoError(t, err)
	require.True(t, again.Replayed)
	require.Equal(t, first.Session.ID, again.Session.ID)
	n := 0
	for _, l := range e.bal.Ledger() {
		if l.EntryType == balance.EntryReserve {
			n++
		}
	}
	require.Equal(t, 1, n, "a replay books nothing")
	require.Equal(t, []string{"ok"}, e.metrics.created, "a replay does not count")
}

func TestCreateKeyReusedDifferentPayload(t *testing.T) {
	e := newEnv(t, 1800)
	_, err := e.svc.Create(context.Background(), e.input("key-0000001"))
	require.NoError(t, err)
	other := e.input("key-0000001")
	other.DurationSeconds = 600
	_, err = e.svc.Create(context.Background(), other)
	require.Equal(t, errs.CodeIdempotencyKeyReused, code(err))
}

func TestCreateSecondSessionDifferentKeySessionAlreadyOpen(t *testing.T) {
	e := newEnv(t, 3600)
	first, err := e.svc.Create(context.Background(), e.input("key-0000001"))
	require.NoError(t, err)
	_, err = e.svc.Create(context.Background(), e.input("key-0000002"))
	require.Equal(t, errs.CodeSessionAlreadyOpen, code(err))
	require.Equal(t, map[string]any{"session_id": first.Session.ID}, errs.DetailsOf(err))
}

func TestCreateInvalidKeyFormat(t *testing.T) {
	e := newEnv(t, 1800)
	for _, k := range []string{"short", "has space in it!", strings.Repeat("a", 129), "ünïcode-key-1234"} {
		_, err := e.svc.Create(context.Background(), e.input(k))
		require.Equal(t, errs.CodeIdempotencyKeyInvalid, code(err), k)
	}
	_, err := e.svc.Create(context.Background(), e.input(""))
	require.Equal(t, errs.CodeIdempotencyKeyRequired, code(err))
	require.Empty(t, e.metrics.created, "client errors are not counted")
}

func TestCreateBalanceInsufficient(t *testing.T) {
	e := newEnv(t, 600)
	_, err := e.svc.Create(context.Background(), e.input("key-0000001"))
	require.Equal(t, errs.CodeBalanceInsufficient, code(err))
	require.Equal(t, map[string]any{"available_seconds": int64(600), "requested_seconds": int64(900)}, errs.DetailsOf(err))
	require.Equal(t, []string{"insufficient_balance"}, e.metrics.created)
	require.Equal(t, int64(600), e.bal.Account(e.userID).AvailableSeconds)
	_, ferr := e.store.Reader().FindOpenByUser(context.Background(), e.userID)
	require.ErrorIs(t, ferr, session.ErrNotFound, "the failed attempt left no session behind")
}

func TestCreateDurationNotAllowed(t *testing.T) {
	e := newEnv(t, 1800)
	in := e.input("key-0000001")
	in.DurationSeconds = 123
	_, err := e.svc.Create(context.Background(), in)
	require.Equal(t, errs.CodeValidationFailed, code(err))
	in = e.input("key-0000001")
	in.ConversationStartMode = "BOTH"
	_, err = e.svc.Create(context.Background(), in)
	require.Equal(t, errs.CodeValidationFailed, code(err))
}

func TestCreateUserSuspendedAndTermsStaleAreCheckedBeforeResolve(t *testing.T) {
	e := newEnv(t, 1800)
	e.users.Set(e.userID, session.UserGateInfo{Status: "SUSPENDED", TermsVersion: terms})
	_, err := e.svc.Create(context.Background(), e.input("key-0000001"))
	require.Equal(t, errs.CodeAccountSuspended, code(err))

	e.users.Set(e.userID, session.UserGateInfo{Status: "ACTIVE", TermsVersion: "2025-01-01"})
	_, err = e.svc.Create(context.Background(), e.input("key-0000001"))
	require.Equal(t, errs.CodeTermsNotAccepted, code(err))
	require.Equal(t, map[string]any{"current_version": terms}, errs.DetailsOf(err))

	require.Zero(t, e.repo.Calls("GetTopic"), "resolve never ran")
	require.Empty(t, e.adaptive.Requests())
}

func TestCreateResolveErrorPropagates(t *testing.T) {
	e := newEnv(t, 1800)
	other := content.Topic{ID: uuid.New(), Code: "OTHER", Name: "Other", Status: content.StatusActive}
	e.repo.PutTopic(other)
	in := e.input("key-0000001")
	in.TopicID = other.ID
	_, err := e.svc.Create(context.Background(), in)
	require.Equal(t, errs.CodeScenarioTopicMismatch, code(err))

	e.character.Status = content.StatusDisabled
	e.putContent()
	_, err = e.svc.Create(context.Background(), e.input("key-0000002"))
	require.Equal(t, errs.CodeCharacterNotAvailable, code(err))
	require.Empty(t, e.metrics.created, "422s are client errors")
}

func TestCreateStoresConfigSnapshot(t *testing.T) {
	e := newEnv(t, 1800)
	res, err := e.svc.Create(context.Background(), e.input("key-0000001"))
	require.NoError(t, err)
	stored := e.store.Session(res.Session.ID)

	var snap struct {
		Character map[string]any `json:"character"`
		Formality string         `json:"formality"`
	}
	require.NoError(t, json.Unmarshal(stored.ConfigSnapshot, &snap))
	require.Equal(t, "KEIGO", snap.Formality)
	for _, k := range []string{"id", "code", "name", "persona", "provider_name", "model_name", "voice_id", "voice"} {
		require.Contains(t, snap.Character, k)
	}
	require.Equal(t, "ELEVENLABS", snap.Character["provider_name"])
	voice := snap.Character["voice"].(map[string]any)
	require.Contains(t, voice, "fallback")
	require.Equal(t, e.character.ID, stored.CharacterID)

	raw := string(stored.ConfigSnapshot) + string(stored.AdaptiveDifficulty)
	for _, banned := range []string{"provider_profile", "voice_profile", "conversation_style", "expression"} {
		require.NotContains(t, raw, banned)
	}
}

func TestCreateSnapshotImmutableAfterCharacterEdit(t *testing.T) {
	e := newEnv(t, 1800)
	res, err := e.svc.Create(context.Background(), e.input("key-0000001"))
	require.NoError(t, err)
	before := string(e.store.Session(res.Session.ID).ConfigSnapshot)
	e.character.Persona = strings.Repeat("A completely different persona. ", 4)
	e.character.VoiceID = "new-voice"
	e.putContent()
	require.Equal(t, before, string(e.store.Session(res.Session.ID).ConfigSnapshot))
	require.Contains(t, before, "A warm guide.")
}

func TestCreateAdaptiveFailure503(t *testing.T) {
	e := newEnv(t, 1800)
	e.adaptive.FailNext("Adaptive", errors.New("db down"))
	_, err := e.svc.Create(context.Background(), e.input("key-0000001"))
	require.Equal(t, errs.CodeServiceUnavailable, code(err))
	require.Equal(t, []string{"error"}, e.metrics.created)
	require.Equal(t, int64(1800), e.bal.Account(e.userID).AvailableSeconds)
}

func TestCreateDatabaseErrorCountsAsError(t *testing.T) {
	e := newEnv(t, 1800)
	e.store.FailNext("Sessions.Insert", errors.New("connection reset"))
	_, err := e.svc.Create(context.Background(), e.input("key-0000001"))
	require.Error(t, err)
	require.Equal(t, []string{"error"}, e.metrics.created)
	require.Equal(t, int64(1800), e.bal.Account(e.userID).AvailableSeconds, "rolled back")
}

func TestCreateUniqueViolationBackstopDecidesOutsideTheTransaction(t *testing.T) {
	// a competing session commits between the in-transaction checks and the INSERT: the unique
	// index fires, the transaction rolls back and the Service decides again from committed state
	t.Run("another open session", func(t *testing.T) {
		e := newEnv(t, 3600)
		rival := session.Session{ID: uuid.Must(uuid.NewV7()), UserID: e.userID, Status: session.StatusReserved, CreateIdempotencyKey: "rival-key-0001", CreateRequestHash: []byte{1}, CreatedAt: t0}
		e.store.BeforeInsert = func() { e.store.PutSession(rival) }
		_, err := e.svc.Create(context.Background(), e.input("key-0000009"))
		require.Equal(t, errs.CodeSessionAlreadyOpen, code(err))
		require.Equal(t, map[string]any{"session_id": rival.ID}, errs.DetailsOf(err))
		require.Equal(t, int64(3600), e.bal.Account(e.userID).AvailableSeconds, "nothing was reserved")
	})
	t.Run("same key won the race", func(t *testing.T) {
		e := newEnv(t, 3600)
		in := e.input("key-0000009")
		winner := session.Session{ID: uuid.Must(uuid.NewV7()), UserID: e.userID, Status: session.StatusReserved, CreateIdempotencyKey: in.IdempotencyKey,
			CreateRequestHash: session.HashCreateRequest(in), CreatedAt: t0}
		e.store.BeforeInsert = func() { e.store.PutSession(winner) }
		res, err := e.svc.Create(context.Background(), in)
		require.NoError(t, err)
		require.True(t, res.Replayed)
		require.Equal(t, winner.ID, res.Session.ID)
	})
	t.Run("same key with another payload won the race", func(t *testing.T) {
		e := newEnv(t, 3600)
		in := e.input("key-0000009")
		winner := session.Session{ID: uuid.Must(uuid.NewV7()), UserID: e.userID, Status: session.StatusReserved, CreateIdempotencyKey: in.IdempotencyKey,
			CreateRequestHash: []byte{9}, CreatedAt: t0}
		e.store.BeforeInsert = func() { e.store.PutSession(winner) }
		_, err := e.svc.Create(context.Background(), in)
		require.Equal(t, errs.CodeIdempotencyKeyReused, code(err))
	})
}

func TestHashCreateRequest(t *testing.T) {
	e := newEnv(t, 0)
	in := e.input("key-0000001")
	h := session.HashCreateRequest(in)
	require.Len(t, h, 32)
	require.Equal(t, h, session.HashCreateRequest(in))

	same := in
	same.IdempotencyKey, same.UserID = "another-key-0002", uuid.New()
	require.Equal(t, h, session.HashCreateRequest(same), "only the five request fields count")

	for name, mod := range map[string]func(*session.CreateSessionInput){
		"character": func(i *session.CreateSessionInput) { i.CharacterID = uuid.New() },
		"mode":      func(i *session.CreateSessionInput) { i.ConversationStartMode = session.StartAIFirst },
		"duration":  func(i *session.CreateSessionInput) { i.DurationSeconds = 600 },
		"topic":     func(i *session.CreateSessionInput) { i.TopicID = uuid.New() },
		"scenario":  func(i *session.CreateSessionInput) { i.ScenarioID = uuid.New() },
	} {
		d := in
		mod(&d)
		require.NotEqual(t, h, session.HashCreateRequest(d), name)
	}
}
