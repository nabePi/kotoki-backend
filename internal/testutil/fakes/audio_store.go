package fakes

import (
	"context"
	"errors"
	"slices"
	"sync"
)

// ErrAudioNotFound is returned by AudioStore.Get for an unknown key.
var ErrAudioNotFound = errors.New("audio not found")

// AudioStore is an in-memory blob store keyed by string, for tests of components that hand audio
// to storage (for instance a TTS cache). No port consumes it yet: the runtime never stores user
// audio (D-099), and the audio packages arrive in later M5 tasks, which may narrow this type.
// FailNext("Put" | "Get" | "Delete", err) fails the next call of a method.
type AudioStore struct {
	FailInjector
	mu    sync.Mutex
	blobs map[string][]byte
}

// NewAudioStore returns an empty store.
func NewAudioStore() *AudioStore { return &AudioStore{blobs: map[string][]byte{}} }

// Put stores a copy of data under key.
func (a *AudioStore) Put(_ context.Context, key string, data []byte) error {
	if err := a.Take("Put"); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.blobs[key] = slices.Clone(data)
	return nil
}

// Get returns a copy of the blob under key.
func (a *AudioStore) Get(_ context.Context, key string) ([]byte, error) {
	if err := a.Take("Get"); err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	b, ok := a.blobs[key]
	if !ok {
		return nil, ErrAudioNotFound
	}
	return slices.Clone(b), nil
}

// Delete removes key; a missing key is not an error.
func (a *AudioStore) Delete(_ context.Context, key string) error {
	if err := a.Take("Delete"); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.blobs, key)
	return nil
}

// Len returns the number of stored blobs.
func (a *AudioStore) Len() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.blobs)
}
