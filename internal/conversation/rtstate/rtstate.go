// Package rtstate is the port of the realtime Redis state (spec 7.9). The runtime talks to Store
// only; redisstore implements it with go-redis and the Lua scripts of 7.9.2. No cgo in here.
package rtstate

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/learner"
)

// RT is the kotoki:session:{id}:rt hash (spec 7.9.1): one field per column. Epoch fields are
// milliseconds; 0 stands for the empty value.
type RT struct {
	Status           string
	TurnState        string
	AcceptingTurns   bool
	ConnID           string
	Connected        bool
	DisconnectedAtMS int64
	Generation       int
	NextTurnSeq      int
	CurrentTurnID    string
	LastServerSeq    int64
	LastDurableSeq   int64
	ExpiresAtMS      int64
	PodID            string
	Fence            int64
	Adaptive         learner.AdaptiveDifficulty
	TurnsSinceAdjust int
	UpdatedAtMS      int64
}

// TurnSnap is the kotoki:session:{id}:turn hash: the turn in progress.
type TurnSnap struct {
	TurnID          uuid.UUID
	TurnSeq         int
	Initiator       string
	TurnState       string
	Generation      int
	UserStartedAtMS int64
	UserText        string
	AssistantText   string
	SpokenSentences int
	UpdatedAtMS     int64
}

// EventEntry is one entry of the kotoki:session:{id}:events stream: a durable event.
type EventEntry struct {
	Seq     int64
	PrevSeq int64 // server_seq of the previous durable event, 0 when none
	Type    string
	Env     []byte // the complete envelope JSON
}

// Summary is the kotoki:session:{id}:summary hash.
type Summary struct {
	Text            string
	CoveredUntilSeq int64
	Model           string
	UpdatedAtMS     int64
}

// Takeover is the message of kotoki:realtime:pod:{pod_id}: {"type":"TAKEOVER",...}.
type Takeover struct {
	SessionID uuid.UUID
	Fence     int64
}

// PodInfo is the kotoki:realtime:podinfo:{pod_id} hash.
type PodInfo struct {
	ActiveSessions int
	Draining       bool
	StartedAt      time.Time
	Build          string
}

// Store is all the Redis state of the realtime runtime. Reads of a missing key report found =
// false without error. Every operation is bounded by the caller's context (200 ms, spec 7.9.4).
type Store interface {
	// AcquireOwner is the Lua script of 7.9.2: false when the stored owner has a newer fence.
	AcquireOwner(ctx context.Context, sid uuid.UUID, pod string, fence int64, ttl time.Duration) (bool, error)
	// ReleaseOwner deletes the owner key only while it still names pod and fence.
	ReleaseOwner(ctx context.Context, sid uuid.UUID, pod string, fence int64) error
	WriteHeartbeat(ctx context.Context, sid uuid.UUID, at time.Time, ttl time.Duration) error
	ReadHeartbeat(ctx context.Context, sid uuid.UUID) (time.Time, bool, error)
	WriteRT(ctx context.Context, sid uuid.UUID, rt RT, ttl time.Duration) error
	ReadRT(ctx context.Context, sid uuid.UUID) (RT, bool, error)
	WriteTurn(ctx context.Context, sid uuid.UUID, t TurnSnap, ttl time.Duration) error
	ReadTurn(ctx context.Context, sid uuid.UUID) (TurnSnap, bool, error)
	DeleteTurn(ctx context.Context, sid uuid.UUID) error
	// AppendEvent is XADD with MAXLEN ~ 500; ReadEvents returns the stream in Seq order.
	AppendEvent(ctx context.Context, sid uuid.UUID, e EventEntry) error
	ReadEvents(ctx context.Context, sid uuid.UUID) ([]EventEntry, error)
	WriteSummary(ctx context.Context, sid uuid.UUID, s Summary, ttl time.Duration) error
	ReadSummary(ctx context.Context, sid uuid.UUID) (Summary, bool, error)
	// ReadMemCtx reads kotoki:session:{id}:memctx (written by memory, M8) as raw JSON.
	ReadMemCtx(ctx context.Context, sid uuid.UUID) ([]byte, bool, error)
	PublishTakeover(ctx context.Context, pod string, t Takeover) error
	// SubscribePod delivers the takeovers published to pod until ctx ends, then closes the channel.
	SubscribePod(ctx context.Context, pod string) (<-chan Takeover, error)
	WritePodInfo(ctx context.Context, pod string, info PodInfo, ttl time.Duration) error
}
