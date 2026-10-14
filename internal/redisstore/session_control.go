package redisstore

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/nabePi/kotoki-backend/internal/session"
)

// ControlChannel is the Pub/Sub channel the owning runtime subscribes to (spec 5.6.6).
func ControlChannel(sessionID uuid.UUID) string {
	return "kotoki:session:" + sessionID.String() + ":control"
}

type sessionControl struct{ rdb redis.UniversalClient }

// NewSessionControl publishes status changes of a session to its control channel.
func NewSessionControl(rdb redis.UniversalClient) session.ControlPublisher {
	return sessionControl{rdb: rdb}
}

// Publish implements session.ControlPublisher.
func (c sessionControl) Publish(ctx context.Context, sessionID uuid.UUID, msg session.ControlMessage) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	return c.rdb.Publish(ctx, ControlChannel(sessionID), b).Err()
}
