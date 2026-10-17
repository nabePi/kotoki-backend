//go:build !windows

package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/csv"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/kotokiclient"
)

// envOr returns the value of an environment variable, "" when unset (flag defaults).
func envOr(key string) string {
	v, _ := os.LookupEnv(key)
	return v
}

type seedOptions struct {
	APIURL, AdminEmail, AdminPassword, Out string
	Count, Balance, Concurrency            int
}

func cmdSeed(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("seed-users", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var o seedOptions
	fs.StringVar(&o.APIURL, "api-url", "http://localhost:8080", "REST API base URL")
	fs.IntVar(&o.Count, "count", 0, "number of users to create (load-1@load.test ... load-N@load.test)")
	fs.IntVar(&o.Balance, "balance", 36000, "seconds of balance granted to each user")
	fs.StringVar(&o.AdminEmail, "admin-email", envOr("KOTOKI_ADMIN_EMAIL"), "admin account (env KOTOKI_ADMIN_EMAIL)")
	fs.StringVar(&o.AdminPassword, "admin-password", envOr("KOTOKI_ADMIN_PASSWORD"), "admin password (env KOTOKI_ADMIN_PASSWORD; prefer the env var)")
	fs.IntVar(&o.Concurrency, "concurrency", 8, "users created in parallel")
	fs.StringVar(&o.Out, "out", "users.csv", "output file: email,password per user (mode 0600)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := seedUsers(ctx, o, func() *kotokiclient.Client { return kotokiclient.New(o.APIURL) }, stdout); err != nil {
		_, _ = fmt.Fprintf(stderr, "kotoki-load seed-users: %v\n", err)
		return 2
	}
	return 0
}

// seedUsers registers Count users through the public API, grants each Balance seconds as the admin,
// and writes email,password rows to Out. The file is written only when every user was created.
func seedUsers(ctx context.Context, o seedOptions, newClient func() *kotokiclient.Client, stdout io.Writer) error {
	switch {
	case o.Count <= 0:
		return fmt.Errorf("--count must be positive")
	case o.Balance <= 0:
		return fmt.Errorf("--balance must be positive")
	case o.AdminEmail == "" || o.AdminPassword == "":
		return fmt.Errorf("--admin-email and --admin-password (or KOTOKI_ADMIN_EMAIL/KOTOKI_ADMIN_PASSWORD) are required")
	case o.Out == "":
		return fmt.Errorf("--out is required")
	}
	workers := min(max(o.Concurrency, 1), o.Count)

	admin := newClient()
	if err := admin.Login(ctx, o.AdminEmail, o.AdminPassword); err != nil {
		return fmt.Errorf("admin login: %w", err)
	}
	terms, err := admin.Terms(ctx)
	if err != nil {
		return fmt.Errorf("terms version: %w", err)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	rows := make([][2]string, o.Count)
	jobs := make(chan int)
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
		done     int
	)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				email, pw, err := seedOne(ctx, newClient(), admin, i+1, terms, int64(o.Balance))
				mu.Lock()
				if err != nil {
					if firstErr == nil {
						firstErr = err
						cancel()
					}
				} else {
					rows[i] = [2]string{email, pw}
					done++
				}
				mu.Unlock()
			}
		}()
	}
feed:
	for i := 0; i < o.Count; i++ {
		select {
		case jobs <- i:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	if done != o.Count {
		return fmt.Errorf("interrupted after %d of %d users", done, o.Count)
	}
	if err := writeUsers(o.Out, rows); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "seeded %d users with %d s of balance each: %s\n", o.Count, o.Balance, o.Out)
	return nil
}

func seedOne(ctx context.Context, c, admin *kotokiclient.Client, n int, terms string, balance int64) (email, password string, err error) {
	email = fmt.Sprintf("load-%d@load.test", n)
	if password, err = randomPassword(); err != nil {
		return "", "", err
	}
	u, err := c.Register(ctx, kotokiclient.RegisterRequest{Email: email, Password: password, FullName: fmt.Sprintf("Load User %d", n), TermsVersion: terms})
	if err != nil {
		return "", "", fmt.Errorf("register %s: %w", email, err)
	}
	id, err := uuid.Parse(u.ID)
	if err != nil {
		return "", "", fmt.Errorf("register %s: user id %q: %w", email, u.ID, err)
	}
	if err := admin.AdminGrantBalance(ctx, id, balance, "load test seed", uuid.NewString()); err != nil {
		return "", "", fmt.Errorf("grant balance to %s: %w", email, err)
	}
	return email, password, nil
}

func randomPassword() (string, error) {
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("random password: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// writeUsers writes the CSV to a temporary file in the same directory and renames it, so a
// half-written users file never exists. The file is 0600: it holds passwords.
func writeUsers(path string, rows [][2]string) (err error) {
	dir := filepath.Dir(path)
	if err = os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".users-*.csv")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(f.Name())
		}
	}()
	w := csv.NewWriter(f)
	_ = w.Write([]string{"email", "password"})
	for _, r := range rows {
		_ = w.Write(r[:])
	}
	w.Flush()
	if err = w.Error(); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
