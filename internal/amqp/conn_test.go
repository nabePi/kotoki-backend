package amqp_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/nabePi/kotoki-backend/internal/amqp"
)

func TestBackoff_Sequence(t *testing.T) {
	mid := func() float64 { return 0.5 }
	var got []time.Duration
	for a := 0; a < 7; a++ {
		got = append(got, amqp.Backoff(a, mid))
	}
	s := time.Second
	assert.Equal(t, []time.Duration{1 * s, 2 * s, 4 * s, 8 * s, 16 * s, 30 * s, 30 * s}, got)

	lo, hi := func() float64 { return 0 }, func() float64 { return 1 }
	assert.Equal(t, 800*time.Millisecond, amqp.Backoff(0, lo))
	assert.Equal(t, 1600*time.Millisecond, amqp.Backoff(1, lo))
	assert.Equal(t, 1200*time.Millisecond, amqp.Backoff(0, hi))
	assert.Equal(t, 36*s, amqp.Backoff(5, hi))

	assert.Equal(t, 1*s, amqp.Backoff(-3, mid))
	assert.Equal(t, 30*s, amqp.Backoff(1000, mid), "huge attempt must not overflow")
}
