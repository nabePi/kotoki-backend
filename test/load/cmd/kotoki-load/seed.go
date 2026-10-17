//go:build !windows

package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/kotokiclient"
	"github.com/nabePi/kotoki-backend/test/load/vu"
)

const (
	// maxSeedAttempts bounds the tries of one call that keeps answering 429 RATE_LIMITED. It is generous
	// on purpose: admin_user (balance grants) refills 2 tokens/s and every worker waits its own
	// Retry-After, so a burst of grants is served at that rate over many short waits.
	maxSeedAttempts = 30
	// maxSeedWait caps one pause between tries (Retry-After of register_ip is up to an hour: waiting
	// that long is a decision for the operator, who should use --spread-ips instead).
	maxSeedWait = 60 * time.Second
)

// envOr returns the value of an environment variable, "" when unset (flag defaults).
func envOr(key string) string {
	v, _ := os.LookupEnv(key)
	return v
}

type seedOptions struct {
	APIURL, AdminEmail, AdminPassword, Out string
	Count, Balance, Concurrency            int
	// SpreadIPs registers user n from the address vu.SpreadIP(n-1) (X-Forwarded-For), the same one
	// `kotoki-load run --spread-ips` gives that VU.
	SpreadIPs bool

	sleep func(ctx context.Context, d time.Duration) error // tests only; default kotokiclient.SleepContext
}

func cmdSeed(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("seed-users", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var o seedOptions
	fs.StringVar(&o.APIURL, "api-url", "http://localhost:8080", "REST API base URL")
	fs.IntVar(&o.Count, "count", 0, "number of users to create (load-1@load.test ... load-N@load.test)")
	fs.IntVar(&o.Balance, "balance", 36000, "seconds of balance granted to each user")
	fs.StringVar(&o.AdminEmail, "admin-email", envOr("KOTOKI_ADMIN_EMAIL"), "admin account (env KOTOKI_ADMIN_EMAIL)")
	// no default from the environment: -h prints flag defaults, and this one is a secret
	fs.StringVar(&o.AdminPassword, "admin-password", "", "admin password; when empty the env KOTOKI_ADMIN_PASSWORD is used (prefer the env var: argv shows in ps)")
	fs.IntVar(&o.Concurrency, "concurrency", 8, "users created in parallel")
	fs.StringVar(&o.Out, "out", "users.csv", "output file: email,password per user (mode 0600)")
	fs.BoolVar(&o.SpreadIPs, "spread-ips", false, "register each user from its own address (X-Forwarded-For) so that the per-IP limits (register 5/h) do not apply; needs TRUSTED_PROXY_CIDRS on the server")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if o.AdminPassword == "" {
		o.AdminPassword = envOr("KOTOKI_ADMIN_PASSWORD")
	}
	if err := seedUsers(ctx, o, func(ip string) *kotokiclient.Client {
		if ip == "" {
			return kotokiclient.New(o.APIURL)
		}
		return kotokiclient.New(o.APIURL, kotokiclient.WithClientIP(ip))
	}, stdout); err != nil {
		_, _ = fmt.Fprintf(stderr, "kotoki-load seed-users: %v\n", err)
		return 2
	}
	return 0
}

// seedUsers registers Count users through the public API, grants each Balance seconds as the admin,
// and writes email,password rows to Out. The file is written only when every user was created.
// newClient builds a client that presents itself as ip ("" = the host's own address). A call the
// server rate-limits (429) is retried after its Retry-After, at most maxSeedAttempts times.
func seedUsers(ctx context.Context, o seedOptions, newClient func(ip string) *kotokiclient.Client, stdout io.Writer) error {
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

	admin := newClient("")
	if err := admin.Login(ctx, o.AdminEmail, o.AdminPassword); err != nil {
		return fmt.Errorf("admin login: %w", err)
	}
	var terms string
	err := retryRateLimited(ctx, o.sleep, func() (e error) { terms, e = admin.Terms(ctx); return e })
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
				ip := ""
				if o.SpreadIPs {
					ip = vu.SpreadIP(i)
				}
				email, pw, err := seedOne(ctx, o.sleep, newClient(ip), admin, i+1, terms, int64(o.Balance))
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

func seedOne(ctx context.Context, sleep func(context.Context, time.Duration) error, c, admin *kotokiclient.Client, n int, terms string, balance int64) (email, password string, err error) {
	email = fmt.Sprintf("load-%d@load.test", n)
	if password, err = randomPassword(); err != nil {
		return "", "", err
	}
	var u kotokiclient.User
	err = retryRateLimited(ctx, sleep, func() (e error) {
		u, e = c.Register(ctx, kotokiclient.RegisterRequest{Email: email, Password: password, FullName: fmt.Sprintf("Load User %d", n), TermsVersion: terms})
		return e
	})
	if err != nil {
		return "", "", fmt.Errorf("register %s: %w", email, err)
	}
	id, err := uuid.Parse(u.ID)
	if err != nil {
		return "", "", fmt.Errorf("register %s: user id %q: %w", email, u.ID, err)
	}
	key := uuid.NewString() // one key for every try of this grant: a replay never grants twice
	if err := retryRateLimited(ctx, sleep, func() error { return admin.AdminGrantBalance(ctx, id, balance, "load test seed", key) }); err != nil {
		return "", "", fmt.Errorf("grant balance to %s: %w", email, err)
	}
	return email, password, nil
}

// retryRateLimited runs f and, while it fails with 429, waits (the answer's Retry-After, else a
// doubling second) and runs it again, at most maxSeedAttempts times in all. Any other error ends it.
func retryRateLimited(ctx context.Context, sleep func(context.Context, time.Duration) error, f func() error) error {
	if sleep == nil {
		sleep = kotokiclient.SleepContext
	}
	var err error
	for attempt := 1; attempt <= maxSeedAttempts; attempt++ {
		if err = f(); err == nil {
			return nil
		}
		var api *kotokiclient.APIError
		if !errors.As(err, &api) || api.Status != http.StatusTooManyRequests || attempt == maxSeedAttempts {
			return err
		}
		wait := api.RetryAfter
		if wait <= 0 {
			wait = time.Second << (attempt - 1)
		}
		if serr := sleep(ctx, min(wait, maxSeedWait)); serr != nil {
			return serr
		}
	}
	return err
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
