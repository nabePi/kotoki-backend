package redisstore

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/nabePi/kotoki-backend/internal/session"
)

// HeartbeatKey is the key the owning runtime refreshes every 5 s (spec 7.9.1): the value is the epoch
// milliseconds of the last time the runtime was alive, the TTL 60 s.
func HeartbeatKey(sessionID uuid.UUID) string { return "kotoki:session:" + sessionID.String() + ":hb" }

type heartbeatReader struct{ rdb redis.UniversalClient }

// NewHeartbeatReader reads runtime heartbeats for the session sweeper. Read with no ids only checks
// that Redis answers (PING), which is how the sweeper's guard probes connectivity.
func NewHeartbeatReader(rdb redis.UniversalClient) session.HeartbeatReader {
	return heartbeatReader{rdb: rdb}
}

// Read implements session.HeartbeatReader: a missing or unparsable key is simply absent from the map.
func (r heartbeatReader) Read(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]time.Time, error) {
	if len(ids) == 0 {
		return map[uuid.UUID]time.Time{}, r.rdb.Ping(ctx).Err()
	}
	keys := make([]string, len(ids))
	for i, id := range ids {
		keys[i] = HeartbeatKey(id)
	}
	vals, err := r.rdb.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]time.Time, len(ids))
	for i, v := range vals {
		s, ok := v.(string)
		if !ok {
			continue
		}
		ms, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			continue
		}
		out[ids[i]] = time.UnixMilli(ms).UTC()
	}
	if len(out) > len(ids) {
		return nil, errors.New("redisstore: heartbeat read returned more keys than asked")
	}
	return out, nil
}
