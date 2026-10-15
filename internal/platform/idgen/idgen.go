// Package idgen provides the UUID generation port.
package idgen

import (
	"encoding/binary"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
)

// Generator produces new UUIDs.
type Generator interface {
	New() uuid.UUID
}

type v7 struct{}

// NewV7 returns a Generator of random UUIDv7 values.
func NewV7() Generator { return v7{} }

func (v7) New() uuid.UUID {
	id, err := uuid.NewV7()
	if err != nil {
		// uuid.NewV7 fails only when the system entropy source fails.
		panic("idgen: entropy source failure: " + err.Error())
	}
	return id
}

type sequence struct {
	n atomic.Uint64
}

// NewSequence returns a deterministic, strictly increasing Generator of valid
// UUIDv7 values for tests.
func NewSequence(t testing.TB) Generator {
	t.Helper()
	return &sequence{}
}

func (s *sequence) New() uuid.UUID {
	n := s.n.Add(1)
	ms := 1760000000000 + n
	var id uuid.UUID
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], ms)
	copy(id[0:6], buf[2:8])
	binary.BigEndian.PutUint64(id[8:16], n)
	id[6] = 0x70
	id[8] = (id[8] & 0x3f) | 0x80
	return id
}
