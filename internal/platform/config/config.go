package config

import (
	"time"

	"github.com/caarlos0/env/v11"
)

// Common holds variables shared by every binary.
type Common struct {
	AppEnv         string `env:"APP_ENV"`
	LogLevel       string `env:"LOG_LEVEL"`
	LogFormat      string `env:"LOG_FORMAT"`
	LogTranscripts bool   `env:"LOG_TRANSCRIPTS"`
	PprofEnabled   bool   `env:"PPROF_ENABLED"`
	MetricsEnabled bool   `env:"METRICS_ENABLED"`
}

// DB holds PostgreSQL pool settings (spec 13.2.3).
type DB struct {
	URL              Secret        `env:"DB_URL"`
	MaxConns         int32         `env:"DB_MAX_CONNS"`
	MinConns         int32         `env:"DB_MIN_CONNS"`
	MaxConnLifetime  time.Duration `env:"DB_MAX_CONN_LIFETIME"`
	MaxConnIdleTime  time.Duration `env:"DB_MAX_CONN_IDLE_TIME"`
	ConnectTimeout   time.Duration `env:"DB_CONNECT_TIMEOUT"`
	StatementTimeout time.Duration `env:"DB_STATEMENT_TIMEOUT"`
	LockTimeout      time.Duration `env:"DB_LOCK_TIMEOUT"`
	IdleInTxTimeout  time.Duration `env:"DB_IDLE_IN_TX_TIMEOUT"`
}

// Redis holds Redis client settings (spec 13.2.4).
type Redis struct {
	URL            Secret        `env:"REDIS_URL"`
	SentinelMaster string        `env:"REDIS_SENTINEL_MASTER"`
	SentinelAddrs  []string      `env:"REDIS_SENTINEL_ADDRS" envSeparator:","`
	Password       Secret        `env:"REDIS_PASSWORD"`
	PoolSize       int           `env:"REDIS_POOL_SIZE"`
	DialTimeout    time.Duration `env:"REDIS_DIAL_TIMEOUT"`
	ReadTimeout    time.Duration `env:"REDIS_READ_TIMEOUT"`
	WriteTimeout   time.Duration `env:"REDIS_WRITE_TIMEOUT"`
}

// OTel holds OpenTelemetry settings.
type OTel struct {
	Endpoint           string `env:"OTEL_EXPORTER_OTLP_ENDPOINT"`
	ServiceName        string `env:"OTEL_SERVICE_NAME"`
	ResourceAttributes string `env:"OTEL_RESOURCE_ATTRIBUTES"`
	TracesSampler      string `env:"OTEL_TRACES_SAMPLER"`
}

// API is the configuration of cmd/api.
type API struct {
	Common
	DB
	Redis
	OTel
	Auth
	Content
	HTTPAddr           string        `env:"HTTP_ADDR"`
	OpsAddr            string        `env:"OPS_ADDR"`
	ShutdownTimeout    time.Duration `env:"SHUTDOWN_TIMEOUT"`
	HTTPMaxBodyBytes   int64         `env:"HTTP_MAX_BODY_BYTES"`
	CORSAllowedOrigins []string      `env:"CORS_ALLOWED_ORIGINS" envSeparator:","`
}

// Realtime is the configuration of cmd/realtime.
type Realtime struct {
	Common
	DB
	Redis
	OTel
	HTTPAddr           string   `env:"HTTP_ADDR"`
	OpsAddr            string   `env:"OPS_ADDR"`
	WSAllowedOrigins   []string `env:"WS_ALLOWED_ORIGINS" envSeparator:","`
	WSAllowEmptyOrigin bool     `env:"WS_ALLOW_EMPTY_ORIGIN"`
	ReadyMaxSessions   int      `env:"REALTIME_READY_MAX_SESSIONS"`
	MaxSessions        int      `env:"REALTIME_MAX_SESSIONS"`
	HardMaxSessions    int      `env:"REALTIME_HARD_MAX_SESSIONS"`
	// TrustedProxyCIDRs is TRUSTED_PROXY_CIDRS (spec 13.2.2): the proxies whose X-Forwarded-For
	// the ws_connect_ip subject honours. Empty → RemoteAddr.
	TrustedProxyCIDRs []string `env:"TRUSTED_PROXY_CIDRS" envSeparator:","`
	RealtimeTuning
}

// Worker is the configuration of cmd/worker.
type Worker struct {
	Common
	DB
	Redis
	OTel
	Rabbit
	Outbox
	Jobs
	Evaluation    WorkerEvaluation
	Summary       WorkerSummary
	HTTPAddr      string        `env:"WORKER_HTTP_ADDR"`
	ShutdownGrace time.Duration `env:"WORKER_SHUTDOWN_GRACE"`
	Handlers      []string      `env:"WORKER_HANDLERS" envSeparator:","`
}

func defaultCommon() Common {
	return Common{AppEnv: "local", LogLevel: "info", LogFormat: "json", MetricsEnabled: true}
}

func defaultDB(maxConns, minConns int32, stmt time.Duration) DB {
	return DB{
		MaxConns: maxConns, MinConns: minConns,
		MaxConnLifetime: 30 * time.Minute, MaxConnIdleTime: 5 * time.Minute,
		ConnectTimeout: 5 * time.Second, StatementTimeout: stmt,
		LockTimeout: 3 * time.Second, IdleInTxTimeout: 15 * time.Second,
	}
}

func defaultRedis(pool int) Redis {
	return Redis{
		PoolSize: pool, DialTimeout: 2 * time.Second,
		ReadTimeout: 500 * time.Millisecond, WriteTimeout: 500 * time.Millisecond,
	}
}

func defaultOTel(service string) OTel {
	return OTel{ServiceName: service, TracesSampler: "always_on"}
}

func defaultAPI() API {
	return API{
		Common: defaultCommon(), DB: defaultDB(10, 2, 5*time.Second),
		Redis: defaultRedis(20), OTel: defaultOTel("kotoki-api"),
		HTTPAddr: ":8080", OpsAddr: ":9090", ShutdownTimeout: 20 * time.Second,
		HTTPMaxBodyBytes:   65536,
		CORSAllowedOrigins: []string{"http://localhost:3000"},
		Auth:               defaultAuth(),
		Content:            defaultContent(),
	}
}

func defaultRealtime() Realtime {
	return Realtime{
		Common: defaultCommon(), DB: defaultDB(6, 2, 5*time.Second),
		Redis: defaultRedis(50), OTel: defaultOTel("kotoki-realtime"),
		HTTPAddr: ":8081", OpsAddr: ":9091",
		WSAllowedOrigins: []string{"http://localhost:3000"},
		ReadyMaxSessions: 95, MaxSessions: 100, HardMaxSessions: 120,
		RealtimeTuning: defaultRealtimeTuning(),
	}
}

func defaultWorker() Worker {
	return Worker{
		Common: defaultCommon(), DB: defaultDB(4, 1, 30*time.Second),
		Redis: defaultRedis(10), OTel: defaultOTel("kotoki-worker"),
		Rabbit: defaultRabbit(), Outbox: defaultOutbox(), Jobs: defaultJobs(),
		Evaluation: defaultWorkerEvaluation(), Summary: defaultWorkerSummary(),
		HTTPAddr: ":8081", ShutdownGrace: 25 * time.Second,
	}
}

// options returns parse options; a nil environ means the process environment.
func options(environ map[string]string) env.Options {
	if environ == nil {
		return env.Options{}
	}
	return env.Options{Environment: environ}
}

// LoadAPI loads and validates the api configuration from the process env.
func LoadAPI() (API, error) { return loadAPI(nil) }

// LoadRealtime loads and validates the realtime configuration.
func LoadRealtime() (Realtime, error) { return loadRealtime(nil) }

// LoadWorker loads and validates the worker configuration.
func LoadWorker() (Worker, error) { return loadWorker(nil) }

func loadAPI(environ map[string]string) (API, error) {
	c := defaultAPI()
	if err := env.ParseWithOptions(&c, options(environ)); err != nil {
		return API{}, err
	}
	if err := c.Validate(); err != nil {
		return API{}, err
	}
	return c, nil
}

func loadRealtime(environ map[string]string) (Realtime, error) {
	c := defaultRealtime()
	if err := env.ParseWithOptions(&c, options(environ)); err != nil {
		return Realtime{}, err
	}
	if err := c.Validate(); err != nil {
		return Realtime{}, err
	}
	return c, nil
}

func loadWorker(environ map[string]string) (Worker, error) {
	c := defaultWorker()
	if err := env.ParseWithOptions(&c, options(environ)); err != nil {
		return Worker{}, err
	}
	if err := c.Validate(); err != nil {
		return Worker{}, err
	}
	return c, nil
}
