package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// serverCreds are the tokens kept for one api-url.
type serverCreds struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	Email        string `json:"email,omitempty"`
}

type credFile struct {
	Servers map[string]serverCreds `json:"servers"`
}

// credStore is $XDG_CONFIG_HOME/kotoki-cli/credentials.json (falling back to
// ~/.config/kotoki-cli), mode 0600, one entry per api-url (§14.11.2).
type credStore struct{ path string }

var credMu sync.Mutex // the read-modify-write below is one critical section per process

func newCredStore(getenv func(string) string) (credStore, error) {
	dir := getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home := getenv("HOME")
		if home == "" {
			h, err := os.UserHomeDir()
			if err != nil {
				return credStore{}, fmt.Errorf("no credentials directory: set XDG_CONFIG_HOME or HOME: %w", err)
			}
			home = h
		}
		dir = filepath.Join(home, ".config")
	}
	return credStore{path: filepath.Join(dir, "kotoki-cli", "credentials.json")}, nil
}

// credKey normalises an api-url so that a trailing slash does not make a second entry.
func credKey(apiURL string) string { return strings.TrimRight(strings.TrimSpace(apiURL), "/") }

func (s credStore) read() (credFile, error) {
	var f credFile
	b, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return credFile{Servers: map[string]serverCreds{}}, nil
	}
	if err != nil {
		return f, fmt.Errorf("read credentials: %w", err)
	}
	// A file that is readable by others (made by hand or by another tool) holds tokens: tighten it.
	if info, serr := os.Stat(s.path); serr == nil && info.Mode().Perm() != 0o600 {
		if cerr := os.Chmod(s.path, 0o600); cerr != nil {
			return f, fmt.Errorf("tighten credentials file mode: %w", cerr)
		}
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return f, fmt.Errorf("credentials file %s is not valid JSON (delete it and log in again): %w", s.path, err)
	}
	if f.Servers == nil {
		f.Servers = map[string]serverCreds{}
	}
	return f, nil
}

// load returns the tokens stored for apiURL (zero value when none).
func (s credStore) load(apiURL string) (serverCreds, error) {
	credMu.Lock()
	defer credMu.Unlock()
	f, err := s.read()
	if err != nil {
		return serverCreds{}, err
	}
	return f.Servers[credKey(apiURL)], nil
}

// save stores the tokens for apiURL and leaves every other entry untouched. The file is replaced
// atomically and always ends up mode 0600 in a 0700 directory.
func (s credStore) save(apiURL string, c serverCreds) error {
	credMu.Lock()
	defer credMu.Unlock()
	f, err := s.read()
	if err != nil {
		return err
	}
	f.Servers[credKey(apiURL)] = c
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create credentials directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".credentials-*.tmp")
	if err != nil {
		return fmt.Errorf("write credentials: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write credentials: %w", err)
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write credentials: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write credentials: %w", err)
	}
	if err := os.Rename(tmp.Name(), s.path); err != nil {
		return fmt.Errorf("write credentials: %w", err)
	}
	return nil
}
