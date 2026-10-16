package amqp

import (
	"errors"
	"strings"
	"testing"

	amqp091 "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/assert"

	"github.com/nabePi/kotoki-backend/internal/events"
)

func TestErrorCode(t *testing.T) {
	assert.Equal(t, "DECODE_ERROR", errorCode(events.Permanent("DECODE_ERROR", errors.New("x"))))
	assert.Equal(t, "ENTITY_NOT_FOUND", errorCode(&events.PermanentError{Code: "ENTITY_NOT_FOUND"}))
	assert.Equal(t, "RETRIES_EXHAUSTED", errorCode(errors.New("boom")))
}

func TestHeaderInt(t *testing.T) {
	h := amqp091.Table{"a": int32(3), "b": int64(2), "c": int(1), "s": "7"}
	assert.Equal(t, 3, headerInt(h, "a"))
	assert.Equal(t, 2, headerInt(h, "b"))
	assert.Equal(t, 1, headerInt(h, "c"))
	assert.Equal(t, 0, headerInt(h, "absent"))
	assert.Equal(t, 0, headerInt(h, "s"))
	assert.Equal(t, 0, headerInt(nil, "a"))
}

func TestTruncate_512(t *testing.T) {
	assert.Equal(t, "short", truncate("short", 512))
	long := strings.Repeat("a", 600)
	assert.Len(t, truncate(long, 512), 512)
	exact := strings.Repeat("b", 512)
	assert.Equal(t, exact, truncate(exact, 512))
	// never splits a multi-byte rune: 3-byte runes, cut inside one
	jp := strings.Repeat("日", 200)
	got := truncate(jp, 512)
	assert.LessOrEqual(t, len(got), 512)
	assert.Equal(t, strings.Repeat("日", 170), got)
}
