package config

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

type validator struct{ errs []error }

func (v *validator) fail(name, format string, args ...any) {
	v.errs = append(v.errs, fmt.Errorf("%s: %s", name, fmt.Sprintf(format, args...)))
}

func (v *validator) err() error { return errors.Join(v.errs...) }

func (v *validator) positive(name string, d time.Duration) {
	if d <= 0 {
		v.fail(name, "must be > 0, got %s", d)
	}
}

func oneOf(s string, set ...string) bool {
	for _, x := range set {
		if s == x {
			return true
		}
	}
	return false
}

func (c Common) validate(v *validator) {
	if !oneOf(c.AppEnv, "local", "dev", "staging", "prod") {
		v.fail("APP_ENV", "must be one of local, dev, staging, prod, got %q", c.AppEnv)
	}
	if !oneOf(c.LogLevel, "debug", "info", "warn", "error") {
		v.fail("LOG_LEVEL", "must be one of debug, info, warn, error, got %q", c.LogLevel)
	}
	if !oneOf(c.LogFormat, "json", "text") {
		v.fail("LOG_FORMAT", "must be json or text, got %q", c.LogFormat)
	} else if c.LogFormat == "text" && c.AppEnv != "local" {
		v.fail("LOG_FORMAT", "text is only allowed when APP_ENV=local")
	}
	if c.LogTranscripts && c.strict() {
		v.fail("LOG_TRANSCRIPTS", "true is rejected when APP_ENV is staging or prod")
	}
}

// strict reports whether APP_ENV is staging or prod.
func (c Common) strict() bool { return c.AppEnv == "staging" || c.AppEnv == "prod" }

func (d DB) validate(v *validator, c Common) {
	if d.URL == "" {
		v.fail("DB_URL", "required")
	} else if c.strict() && strings.Contains(d.URL.Reveal(), "sslmode=disable") {
		v.fail("DB_URL", "sslmode=disable is rejected when APP_ENV is staging or prod")
	}
	if d.MaxConns <= 0 {
		v.fail("DB_MAX_CONNS", "must be > 0, got %d", d.MaxConns)
	}
	if d.MinConns < 0 {
		v.fail("DB_MIN_CONNS", "must be >= 0, got %d", d.MinConns)
	}
	if d.MinConns > d.MaxConns {
		v.fail("DB_MIN_CONNS", "%d exceeds DB_MAX_CONNS %d", d.MinConns, d.MaxConns)
	}
	v.positive("DB_MAX_CONN_LIFETIME", d.MaxConnLifetime)
	v.positive("DB_MAX_CONN_IDLE_TIME", d.MaxConnIdleTime)
	v.positive("DB_CONNECT_TIMEOUT", d.ConnectTimeout)
	v.positive("DB_STATEMENT_TIMEOUT", d.StatementTimeout)
	v.positive("DB_LOCK_TIMEOUT", d.LockTimeout)
	v.positive("DB_IDLE_IN_TX_TIMEOUT", d.IdleInTxTimeout)
}

func (r Redis) validate(v *validator, c Common) {
	if r.SentinelMaster != "" {
		if len(r.SentinelAddrs) == 0 {
			v.fail("REDIS_SENTINEL_ADDRS", "required when REDIS_SENTINEL_MASTER is set")
		}
	} else if r.URL == "" {
		v.fail("REDIS_URL", "required unless REDIS_SENTINEL_MASTER is set")
	} else if c.strict() && !strings.HasPrefix(r.URL.Reveal(), "rediss://") {
		v.fail("REDIS_URL", "must use rediss:// when APP_ENV is staging or prod")
	}
	if r.PoolSize <= 0 {
		v.fail("REDIS_POOL_SIZE", "must be > 0, got %d", r.PoolSize)
	}
	v.positive("REDIS_DIAL_TIMEOUT", r.DialTimeout)
	v.positive("REDIS_READ_TIMEOUT", r.ReadTimeout)
	v.positive("REDIS_WRITE_TIMEOUT", r.WriteTimeout)
}

// validateOrigins enforces spec 13.2.13 for CORS/WS origin lists. Empty is
// rejected outside local/dev; "*" is always rejected; staging/prod require
// https; local/dev additionally allow explicit http://localhost[:port].
func validateOrigins(v *validator, name string, origins []string, c Common) {
	if len(origins) == 0 {
		if c.AppEnv != "local" && c.AppEnv != "dev" {
			v.fail(name, "must not be empty when APP_ENV=%s", c.AppEnv)
		}
		return
	}
	for _, o := range origins {
		if o == "*" || strings.Contains(o, "*") {
			v.fail(name, "wildcard origin %q is not allowed", o)
			continue
		}
		u, err := url.Parse(o)
		if err != nil || u.Host == "" || u.Path != "" && u.Path != "/" {
			v.fail(name, "invalid origin %q (want scheme://host[:port])", o)
			continue
		}
		switch {
		case u.Scheme == "https":
		case u.Scheme == "http" && !c.strict() && u.Hostname() == "localhost":
		default:
			v.fail(name, "origin %q must be https (http://localhost allowed only in local/dev)", o)
		}
	}
}

func validateHandlers(v *validator, h []string) {
	for _, n := range h {
		if !workerHandlerNames[n] {
			v.fail("WORKER_HANDLERS", "unknown handler %q", n)
		}
	}
}

// Validate applies the startup rules of spec 13.2.13 for the api binary.
func (c API) Validate() error {
	v := &validator{}
	c.Common.validate(v)
	c.DB.validate(v, c.Common)
	c.Redis.validate(v, c.Common)
	validateOrigins(v, "CORS_ALLOWED_ORIGINS", c.CORSAllowedOrigins, c.Common)
	v.positive("SHUTDOWN_TIMEOUT", c.ShutdownTimeout)
	if c.HTTPMaxBodyBytes <= 0 {
		v.fail("HTTP_MAX_BODY_BYTES", "must be > 0, got %d", c.HTTPMaxBodyBytes)
	}
	return v.err()
}

// Validate applies the startup rules of spec 13.2.13 for the realtime binary.
func (c Realtime) Validate() error {
	v := &validator{}
	c.Common.validate(v)
	c.DB.validate(v, c.Common)
	c.Redis.validate(v, c.Common)
	validateOrigins(v, "WS_ALLOWED_ORIGINS", c.WSAllowedOrigins, c.Common)
	if c.WSAllowEmptyOrigin && c.strict() {
		v.fail("WS_ALLOW_EMPTY_ORIGIN", "true is rejected when APP_ENV is staging or prod")
	}
	if c.ReadyMaxSessions <= 0 {
		v.fail("REALTIME_READY_MAX_SESSIONS", "must be > 0, got %d", c.ReadyMaxSessions)
	}
	if c.ReadyMaxSessions > c.MaxSessions {
		v.fail("REALTIME_READY_MAX_SESSIONS", "%d exceeds REALTIME_MAX_SESSIONS %d", c.ReadyMaxSessions, c.MaxSessions)
	}
	if c.MaxSessions > c.HardMaxSessions {
		v.fail("REALTIME_MAX_SESSIONS", "%d exceeds REALTIME_HARD_MAX_SESSIONS %d", c.MaxSessions, c.HardMaxSessions)
	}
	return v.err()
}

// Validate applies the startup rules of spec 13.2.13 for the worker binary.
// WORKER_HANDLERS may be empty here: the --handlers flag can supply it.
func (c Worker) Validate() error {
	v := &validator{}
	c.Common.validate(v)
	c.DB.validate(v, c.Common)
	c.Redis.validate(v, c.Common)
	v.positive("WORKER_SHUTDOWN_GRACE", c.ShutdownGrace)
	validateHandlers(v, c.Handlers)
	return v.err()
}
