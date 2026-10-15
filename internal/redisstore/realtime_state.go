package redisstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/nabePi/kotoki-backend/internal/conversation/rtstate"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
)

// rtOpTimeout bounds every Redis operation of the realtime state (spec 7.9.4).
const rtOpTimeout = 200 * time.Millisecond

// eventsExpire is the EXPIRE set on the replay stream with every XADD, and eventsMaxLen its
// approximate MAXLEN (spec 7.9.1, D-083).
const (
	eventsExpire = 120 * time.Second
	eventsMaxLen = 500
)

// Subscribe backoff after the Pub/Sub connection drops: 100 ms doubling up to 1 s (spec 7.9.4).
const (
	subBackoffMin = 100 * time.Millisecond
	subBackoffMax = time.Second
)

// ownerAcquireLua is the acquire / refresh / takeover script of spec 7.9.2, verbatim.
var ownerAcquireLua = redis.NewScript(`
local cur = redis.call('GET', KEYS[1])
if cur then
  local f = tonumber(string.match(cur, '|(%d+)$'))
  if f and f > tonumber(ARGV[2]) then return 0 end
end
redis.call('SET', KEYS[1], ARGV[1] .. '|' .. ARGV[2], 'PX', ARGV[3])
return 1
`)

// ownerReleaseLua is the release script of spec 7.9.2, verbatim. ARGV[1] is "{pod_id}|{fence}".
var ownerReleaseLua = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then return redis.call('DEL', KEYS[1]) end
return 0
`)

// RealtimeState implements rtstate.Store on Redis (spec 7.9). Script.Run uses EVALSHA and falls
// back to EVAL on NOSCRIPT.
type RealtimeState struct {
	rdb redis.UniversalClient
	log *slog.Logger
}

var _ rtstate.Store = (*RealtimeState)(nil)

// NewRealtimeState returns the Redis rtstate.Store. A nil log discards the resubscribe warnings.
func NewRealtimeState(rdb redis.UniversalClient, log *slog.Logger) *RealtimeState {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &RealtimeState{rdb: rdb, log: log}
}

func sessionKey(sid uuid.UUID, suffix string) string {
	return "kotoki:session:" + sid.String() + ":" + suffix
}

func podChannel(pod string) string { return "kotoki:realtime:pod:" + pod }

func podInfoKey(pod string) string { return "kotoki:realtime:podinfo:" + pod }

// op bounds one operation to rtOpTimeout.
func op(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, rtOpTimeout)
}

func wrapRT(err error, what string) error {
	if err == nil {
		return nil
	}
	return errs.Wrap(err, errs.CodeInternalError, "realtime state: "+what)
}

func checkTTL(ttl time.Duration) error {
	if ttl <= 0 {
		return errs.New(errs.CodeInternalError, "realtime state: ttl must be positive")
	}
	return nil
}

func flag(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

func ownerValue(pod string, fence int64) string { return pod + "|" + strconv.FormatInt(fence, 10) }

// AcquireOwner implements rtstate.Store with the script of spec 7.9.2.
func (s *RealtimeState) AcquireOwner(ctx context.Context, sid uuid.UUID, pod string, fence int64, ttl time.Duration) (bool, error) {
	if err := checkTTL(ttl); err != nil {
		return false, err
	}
	ctx, cancel := op(ctx)
	defer cancel()
	n, err := ownerAcquireLua.Run(ctx, s.rdb, []string{sessionKey(sid, "owner")}, pod, fence, ttl.Milliseconds()).Int()
	if err != nil {
		return false, wrapRT(err, "acquire owner")
	}
	return n == 1, nil
}

// ReleaseOwner implements rtstate.Store.
func (s *RealtimeState) ReleaseOwner(ctx context.Context, sid uuid.UUID, pod string, fence int64) error {
	ctx, cancel := op(ctx)
	defer cancel()
	err := ownerReleaseLua.Run(ctx, s.rdb, []string{sessionKey(sid, "owner")}, ownerValue(pod, fence)).Err()
	if err != nil && !errors.Is(err, redis.Nil) {
		return wrapRT(err, "release owner")
	}
	return nil
}

// WriteHeartbeat implements rtstate.Store: the value is the epoch milliseconds.
func (s *RealtimeState) WriteHeartbeat(ctx context.Context, sid uuid.UUID, at time.Time, ttl time.Duration) error {
	if err := checkTTL(ttl); err != nil {
		return err
	}
	ctx, cancel := op(ctx)
	defer cancel()
	return wrapRT(s.rdb.Set(ctx, HeartbeatKey(sid), strconv.FormatInt(at.UnixMilli(), 10), ttl).Err(), "write heartbeat")
}

// ReadHeartbeat implements rtstate.Store.
func (s *RealtimeState) ReadHeartbeat(ctx context.Context, sid uuid.UUID) (time.Time, bool, error) {
	ctx, cancel := op(ctx)
	defer cancel()
	v, err := s.rdb.Get(ctx, HeartbeatKey(sid)).Result()
	if errors.Is(err, redis.Nil) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, wrapRT(err, "read heartbeat")
	}
	ms, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return time.Time{}, false, wrapRT(err, "parse heartbeat")
	}
	return time.UnixMilli(ms).UTC(), true, nil
}

// writeHash replaces the whole hash and sets its TTL in one MULTI, so a key never lives without TTL.
func (s *RealtimeState) writeHash(ctx context.Context, key string, vals map[string]any, ttl time.Duration, what string) error {
	if err := checkTTL(ttl); err != nil {
		return err
	}
	ctx, cancel := op(ctx)
	defer cancel()
	_, err := s.rdb.TxPipelined(ctx, func(p redis.Pipeliner) error {
		p.HSet(ctx, key, vals)
		p.PExpire(ctx, key, ttl)
		return nil
	})
	return wrapRT(err, what)
}

// readHash returns the hash and whether the key exists.
func (s *RealtimeState) readHash(ctx context.Context, key, what string) (hashReader, bool, error) {
	ctx, cancel := op(ctx)
	defer cancel()
	m, err := s.rdb.HGetAll(ctx, key).Result()
	if err != nil {
		return hashReader{}, false, wrapRT(err, what)
	}
	return hashReader{m: m}, len(m) > 0, nil
}

// hashReader parses hash fields, keeping the first error. A missing field reads as its zero value.
type hashReader struct {
	m   map[string]string
	err error
}

func (h *hashReader) str(k string) string { return h.m[k] }

func (h *hashReader) int64(k string) int64 {
	v := h.m[k]
	if v == "" {
		return 0
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil && h.err == nil {
		h.err = fmt.Errorf("field %s: %w", k, err)
	}
	return n
}

func (h *hashReader) int(k string) int { return int(h.int64(k)) }

func (h *hashReader) bool(k string) bool { return h.m[k] == "1" }

// WriteRT implements rtstate.Store: HSET of every field of spec 7.9.1 (0 epoch writes "").
func (s *RealtimeState) WriteRT(ctx context.Context, sid uuid.UUID, rt rtstate.RT, ttl time.Duration) error {
	adaptive, err := json.Marshal(rt.Adaptive)
	if err != nil {
		return wrapRT(err, "marshal adaptive")
	}
	disc := ""
	if rt.DisconnectedAtMS != 0 {
		disc = strconv.FormatInt(rt.DisconnectedAtMS, 10)
	}
	return s.writeHash(ctx, sessionKey(sid, "rt"), map[string]any{
		"status": rt.Status, "turn_state": rt.TurnState, "accepting_turns": flag(rt.AcceptingTurns),
		"conn_id": rt.ConnID, "connected": flag(rt.Connected), "disconnected_at": disc,
		"generation": rt.Generation, "next_turn_seq": rt.NextTurnSeq, "current_turn_id": rt.CurrentTurnID,
		"last_server_seq": rt.LastServerSeq, "last_durable_seq": rt.LastDurableSeq, "expires_at": rt.ExpiresAtMS,
		"pod_id": rt.PodID, "fence": rt.Fence, "adaptive": string(adaptive),
		"turns_since_adjust": rt.TurnsSinceAdjust, "updated_at": rt.UpdatedAtMS,
	}, ttl, "write rt")
}

// ReadRT implements rtstate.Store.
func (s *RealtimeState) ReadRT(ctx context.Context, sid uuid.UUID) (rtstate.RT, bool, error) {
	h, found, err := s.readHash(ctx, sessionKey(sid, "rt"), "read rt")
	if err != nil || !found {
		return rtstate.RT{}, false, err
	}
	rt := rtstate.RT{
		Status: h.str("status"), TurnState: h.str("turn_state"), AcceptingTurns: h.bool("accepting_turns"),
		ConnID: h.str("conn_id"), Connected: h.bool("connected"), DisconnectedAtMS: h.int64("disconnected_at"),
		Generation: h.int("generation"), NextTurnSeq: h.int("next_turn_seq"), CurrentTurnID: h.str("current_turn_id"),
		LastServerSeq: h.int64("last_server_seq"), LastDurableSeq: h.int64("last_durable_seq"), ExpiresAtMS: h.int64("expires_at"),
		PodID: h.str("pod_id"), Fence: h.int64("fence"), TurnsSinceAdjust: h.int("turns_since_adjust"), UpdatedAtMS: h.int64("updated_at"),
	}
	if raw := h.str("adaptive"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &rt.Adaptive); err != nil && h.err == nil {
			h.err = fmt.Errorf("field adaptive: %w", err)
		}
	}
	if h.err != nil {
		return rtstate.RT{}, false, wrapRT(h.err, "parse rt")
	}
	return rt, true, nil
}

// WriteTurn implements rtstate.Store.
func (s *RealtimeState) WriteTurn(ctx context.Context, sid uuid.UUID, t rtstate.TurnSnap, ttl time.Duration) error {
	return s.writeHash(ctx, sessionKey(sid, "turn"), map[string]any{
		"turn_id": t.TurnID.String(), "turn_seq": t.TurnSeq, "initiator": t.Initiator, "turn_state": t.TurnState,
		"generation": t.Generation, "user_started_at": t.UserStartedAtMS, "user_text": t.UserText,
		"assistant_text": t.AssistantText, "spoken_sentences": t.SpokenSentences, "updated_at": t.UpdatedAtMS,
	}, ttl, "write turn")
}

// ReadTurn implements rtstate.Store.
func (s *RealtimeState) ReadTurn(ctx context.Context, sid uuid.UUID) (rtstate.TurnSnap, bool, error) {
	h, found, err := s.readHash(ctx, sessionKey(sid, "turn"), "read turn")
	if err != nil || !found {
		return rtstate.TurnSnap{}, false, err
	}
	t := rtstate.TurnSnap{
		TurnSeq: h.int("turn_seq"), Initiator: h.str("initiator"), TurnState: h.str("turn_state"),
		Generation: h.int("generation"), UserStartedAtMS: h.int64("user_started_at"), UserText: h.str("user_text"),
		AssistantText: h.str("assistant_text"), SpokenSentences: h.int("spoken_sentences"), UpdatedAtMS: h.int64("updated_at"),
	}
	id, perr := uuid.Parse(h.str("turn_id"))
	if perr != nil && h.err == nil {
		h.err = fmt.Errorf("field turn_id: %w", perr)
	}
	t.TurnID = id
	if h.err != nil {
		return rtstate.TurnSnap{}, false, wrapRT(h.err, "parse turn")
	}
	return t, true, nil
}

// DeleteTurn implements rtstate.Store.
func (s *RealtimeState) DeleteTurn(ctx context.Context, sid uuid.UUID) error {
	ctx, cancel := op(ctx)
	defer cancel()
	return wrapRT(s.rdb.Del(ctx, sessionKey(sid, "turn")).Err(), "delete turn")
}

// AppendEvent implements rtstate.Store: XADD MAXLEN ~ 500, then EXPIRE 120 in the same MULTI.
func (s *RealtimeState) AppendEvent(ctx context.Context, sid uuid.UUID, e rtstate.EventEntry) error {
	ctx, cancel := op(ctx)
	defer cancel()
	key := sessionKey(sid, "events")
	_, err := s.rdb.TxPipelined(ctx, func(p redis.Pipeliner) error {
		p.XAdd(ctx, &redis.XAddArgs{
			Stream: key, MaxLen: eventsMaxLen, Approx: true,
			Values: map[string]any{"seq": e.Seq, "prev_seq": e.PrevSeq, "type": e.Type, "env": string(e.Env)},
		})
		p.Expire(ctx, key, eventsExpire)
		return nil
	})
	return wrapRT(err, "append event")
}

// ReadEvents implements rtstate.Store; the entries are returned in Seq order.
func (s *RealtimeState) ReadEvents(ctx context.Context, sid uuid.UUID) ([]rtstate.EventEntry, error) {
	ctx, cancel := op(ctx)
	defer cancel()
	msgs, err := s.rdb.XRange(ctx, sessionKey(sid, "events"), "-", "+").Result()
	if err != nil {
		return nil, wrapRT(err, "read events")
	}
	out := make([]rtstate.EventEntry, 0, len(msgs))
	for _, m := range msgs {
		h := hashReader{m: make(map[string]string, len(m.Values))}
		for k, v := range m.Values {
			if sv, ok := v.(string); ok {
				h.m[k] = sv
			}
		}
		e := rtstate.EventEntry{Seq: h.int64("seq"), PrevSeq: h.int64("prev_seq"), Type: h.str("type"), Env: []byte(h.str("env"))}
		if h.err != nil {
			return nil, wrapRT(h.err, "parse event")
		}
		out = append(out, e)
	}
	slices.SortStableFunc(out, func(a, b rtstate.EventEntry) int {
		switch {
		case a.Seq < b.Seq:
			return -1
		case a.Seq > b.Seq:
			return 1
		}
		return 0
	})
	return out, nil
}

// WriteSummary implements rtstate.Store.
func (s *RealtimeState) WriteSummary(ctx context.Context, sid uuid.UUID, sum rtstate.Summary, ttl time.Duration) error {
	return s.writeHash(ctx, sessionKey(sid, "summary"), map[string]any{
		"text": sum.Text, "covered_until_seq": sum.CoveredUntilSeq, "model": sum.Model, "updated_at": sum.UpdatedAtMS,
	}, ttl, "write summary")
}

// ReadSummary implements rtstate.Store.
func (s *RealtimeState) ReadSummary(ctx context.Context, sid uuid.UUID) (rtstate.Summary, bool, error) {
	h, found, err := s.readHash(ctx, sessionKey(sid, "summary"), "read summary")
	if err != nil || !found {
		return rtstate.Summary{}, false, err
	}
	sum := rtstate.Summary{Text: h.str("text"), CoveredUntilSeq: h.int64("covered_until_seq"), Model: h.str("model"), UpdatedAtMS: h.int64("updated_at")}
	if h.err != nil {
		return rtstate.Summary{}, false, wrapRT(h.err, "parse summary")
	}
	return sum, true, nil
}

// ReadMemCtx implements rtstate.Store: the raw JSON memory (M8) writes under :memctx.
func (s *RealtimeState) ReadMemCtx(ctx context.Context, sid uuid.UUID) ([]byte, bool, error) {
	ctx, cancel := op(ctx)
	defer cancel()
	b, err := s.rdb.Get(ctx, sessionKey(sid, "memctx")).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, wrapRT(err, "read memctx")
	}
	return b, true, nil
}

type takeoverBody struct {
	Type      string `json:"type"`
	SessionID string `json:"session_id"`
	Fence     int64  `json:"fence"`
}

// PublishTakeover implements rtstate.Store: {"type":"TAKEOVER","session_id":"…","fence":42}.
func (s *RealtimeState) PublishTakeover(ctx context.Context, pod string, t rtstate.Takeover) error {
	b, err := json.Marshal(takeoverBody{Type: "TAKEOVER", SessionID: t.SessionID.String(), Fence: t.Fence})
	if err != nil {
		return wrapRT(err, "marshal takeover")
	}
	ctx, cancel := op(ctx)
	defer cancel()
	return wrapRT(s.rdb.Publish(ctx, podChannel(pod), b).Err(), "publish takeover")
}

// SubscribePod implements rtstate.Store. The subscription is confirmed before it returns; when the
// connection drops it resubscribes with a backoff of at most 1 s (spec 7.9.4). Messages published
// while it is down are lost: the Postgres fence keeps takeover safe.
func (s *RealtimeState) SubscribePod(ctx context.Context, pod string) (<-chan rtstate.Takeover, error) {
	ps, err := s.subscribe(ctx, pod)
	if err != nil {
		return nil, wrapRT(err, "subscribe pod")
	}
	out := make(chan rtstate.Takeover, 16)
	go s.pumpTakeovers(ctx, pod, ps, out)
	return out, nil
}

func (s *RealtimeState) subscribe(ctx context.Context, pod string) (*redis.PubSub, error) {
	cctx, cancel := op(ctx)
	defer cancel()
	ps := s.rdb.Subscribe(cctx, podChannel(pod))
	if _, err := ps.Receive(cctx); err != nil {
		_ = ps.Close()
		return nil, err
	}
	return ps, nil
}

func (s *RealtimeState) pumpTakeovers(ctx context.Context, pod string, ps *redis.PubSub, out chan<- rtstate.Takeover) {
	defer close(out)
	backoff := subBackoffMin
	for ps != nil && ctx.Err() == nil {
		// Closing ps unblocks ReceiveMessage whatever the client's ContextTimeoutEnabled is.
		stop := context.AfterFunc(ctx, func() { _ = ps.Close() })
		err := s.forwardTakeovers(ctx, ps, out)
		stop()
		_ = ps.Close()
		if ctx.Err() != nil {
			return
		}
		s.log.Warn("realtime state: pod subscription lost; resubscribing", "pod", pod, "err", err)
		ps = nil
		for ps == nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, subBackoffMax)
			if p, err := s.subscribe(ctx, pod); err == nil {
				ps, backoff = p, subBackoffMin
			}
		}
	}
}

// forwardTakeovers relays messages until the subscription errors or ctx ends.
func (s *RealtimeState) forwardTakeovers(ctx context.Context, ps *redis.PubSub, out chan<- rtstate.Takeover) error {
	for {
		msg, err := ps.ReceiveMessage(ctx)
		if err != nil {
			return err
		}
		var b takeoverBody
		if json.Unmarshal([]byte(msg.Payload), &b) != nil || b.Type != "TAKEOVER" {
			continue
		}
		sid, perr := uuid.Parse(b.SessionID)
		if perr != nil {
			continue
		}
		select {
		case out <- rtstate.Takeover{SessionID: sid, Fence: b.Fence}:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// WritePodInfo implements rtstate.Store.
func (s *RealtimeState) WritePodInfo(ctx context.Context, pod string, info rtstate.PodInfo, ttl time.Duration) error {
	return s.writeHash(ctx, podInfoKey(pod), map[string]any{
		"active_sessions": info.ActiveSessions, "draining": flag(info.Draining),
		"started_at": info.StartedAt.UnixMilli(), "build": info.Build,
	}, ttl, "write podinfo")
}
