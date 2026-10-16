//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/nabePi/kotoki-backend/internal/testutil"
)

const (
	// testOrigin is the Origin the clients send; the realtime process allows it.
	testOrigin = "http://localhost:3000"
	// readyTimeout is how long a subprocess may take to answer /readyz 200 (spec 14.5).
	readyTimeout = 30 * time.Second
)

// stack is the running system under test; it is set by TestMain.
var stack *env

// env is the e2e stack: shared Postgres and Redis, one cmd/api and one cmd/realtime process.
type env struct {
	root      string
	pool      *pgxpool.Pool
	rdb       *redis.Client
	pgDSN     string
	redisURL  string
	fixtures  string
	binDir    string
	apiBin    string
	rtBin     string
	logDir    string
	api       *proc
	rt        *proc
	apiURL    string // http://host:port
	wsURL     string // ws://host:port/api/v1/realtime
	workerMu  sync.Mutex
	workerBin string
	procMu    sync.Mutex
	procs     []*proc
	catalogMu sync.Mutex
	catalog   *catalogChoice
}

// proc is one subprocess (a real binary).
type proc struct {
	name    string
	cmd     *exec.Cmd
	logPath string
	logFile *os.File
	exited  chan error
	opsAddr string
	addr    string // public listener
}

func setup(tb *mainTB) *env {
	e := &env{root: testutil.ModuleRoot(tb)}
	var err error
	if e.logDir, err = os.MkdirTemp("", "kotoki-e2e-"); err != nil {
		tb.Fatalf("temp dir: %v", err)
	}
	tb.cleanup(func() { _ = os.RemoveAll(e.logDir) })

	e.pool = testutil.NewPostgres(tb)
	e.pgDSN = e.pool.Config().ConnString()
	seed(tb, e)
	e.rdb = testutil.NewRedis(tb)
	e.redisURL = "redis://" + e.rdb.Options().Addr + "/0"

	e.fixtures = filepath.Join(e.logDir, "fixtures16k")
	gen := exec.Command("go", "run", "./test/tools/genaudio", "-to16k", e.fixtures, "-src", "testdata/audio/synth")
	gen.Dir = e.root
	if out, err := gen.CombinedOutput(); err != nil {
		tb.Fatalf("genaudio -to16k: %v\n%s", err, out)
	}

	e.binDir = filepath.Join(e.logDir, "bin")
	e.apiBin = e.build(tb, "api")
	e.rtBin = e.build(tb, "realtime")

	e.api = e.startAPI(tb)
	e.rt = e.startRealtime(tb, "e2e-pod-1")
	e.api.waitReady(tb)
	e.rt.waitReady(tb)
	e.apiURL = "http://" + e.api.addr
	e.wsURL = "ws://" + e.rt.addr + "/api/v1/realtime"
	return e
}

// seed applies seed/*.sql in name order, like `make seed`.
func seed(tb testing.TB, e *env) {
	files, err := filepath.Glob(filepath.Join(e.root, "seed", "*.sql"))
	if err != nil || len(files) == 0 {
		tb.Fatalf("seed files: %v (%d found)", err, len(files))
	}
	sort.Strings(files)
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			tb.Fatalf("read %s: %v", f, err)
		}
		if _, err := e.pool.Exec(context.Background(), string(b)); err != nil {
			tb.Fatalf("seed %s: %v", f, err)
		}
	}
}

// build compiles ./cmd/<name> with the race detector (spec 14.5). cmd/realtime needs cgo anyway;
// -race needs it for the API too.
func (e *env) build(tb testing.TB, name string) string {
	out := filepath.Join(e.binDir, name)
	c := exec.Command("go", "build", "-race", "-o", out, "./cmd/"+name)
	c.Dir = e.root
	c.Env = append(os.Environ(), "CGO_ENABLED=1")
	if b, err := c.CombinedOutput(); err != nil {
		tb.Fatalf("go build ./cmd/%s: %v\n%s", name, err, b)
	}
	return out
}

// baseEnv is the explicit environment of every subprocess: nothing of the developer's own
// environment but PATH leaks in (spec 14.5).
func (e *env) baseEnv(tb testing.TB, extra map[string]string) []string {
	m := testutil.MinimalEnv(tb, map[string]string{
		"DB_URL": e.pgDSN, "REDIS_URL": e.redisURL, "LOG_FORMAT": "json", "LOG_LEVEL": "info",
		"PROVIDER_MODE": "fake", "INITIAL_BALANCE_SECONDS": "3600", "WS_ALLOW_EMPTY_ORIGIN": "true",
		"WS_ALLOWED_ORIGINS": testOrigin, "CORS_ALLOWED_ORIGINS": testOrigin,
		"FAKE_STT_FIXTURE_DIR":        e.fixtures,
		"FAKE_PRIMARY_STT_LATENCY_MS": "250", "FAKE_PRIMARY_LLM_LATENCY_MS": "500",
		"FAKE_LLM_TOKEN_INTERVAL_MS": "30", "FAKE_PRIMARY_TTS_LATENCY_MS": "300",
		// The fault tests set and clear kotoki:fake:faults and move on within seconds; a fast poll
		// keeps a pod from still holding the last test's fault when the next one begins.
		"FAKE_FAULTS_POLL_INTERVAL": "100ms",
	}, extra)
	out := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + e.logDir}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, k+"="+m[k])
	}
	return out
}

func (e *env) startAPI(tb testing.TB) *proc {
	p := &proc{name: "api", addr: testutil.FreeAddr(tb), opsAddr: testutil.FreeAddr(tb)}
	return e.launch(tb, p, e.apiBin, e.baseEnv(tb, map[string]string{"HTTP_ADDR": p.addr, "OPS_ADDR": p.opsAddr}))
}

// startRealtime starts a cmd/realtime process with the given pod id (more than one is how the
// takeover and drain scenarios run).
func (e *env) startRealtime(tb testing.TB, podID string) *proc {
	p := &proc{name: "realtime-" + podID, addr: testutil.FreeAddr(tb), opsAddr: testutil.FreeAddr(tb)}
	return e.launch(tb, p, e.rtBin, e.baseEnv(tb, map[string]string{
		"HTTP_ADDR": p.addr, "OPS_ADDR": p.opsAddr, "REALTIME_POD_ID": podID,
	}))
}

// startWorker starts a cmd/worker process with the given handler flags (WORKER_HANDLERS) against the
// shared Postgres and Redis and the RabbitMQ at rabbitURL, and waits until it is ready. The binary
// is built on first use: only the M6 scenarios need a worker, and a worker left running in every
// test would sweep the sessions other scenarios leave ACTIVE on purpose. The sweeper tick is 2 s
// so a scenario does not wait for the 10 s production interval. The caller stops it with p.stop.
func (e *env) startWorker(tb testing.TB, rabbitURL, handlers string) *proc {
	e.workerMu.Lock()
	if e.workerBin == "" {
		e.workerBin = e.build(tb, "worker")
	}
	bin := e.workerBin
	e.workerMu.Unlock()
	p := &proc{name: "worker-" + strings.ReplaceAll(handlers, ",", "-"), opsAddr: testutil.FreeAddr(tb)}
	p = e.launch(tb, p, bin, e.baseEnv(tb, map[string]string{
		"WORKER_HTTP_ADDR": p.opsAddr, "WORKER_HANDLERS": handlers, "RABBITMQ_URL": rabbitURL,
		"SWEEPER_INTERVAL": "2s", "WORKER_SHUTDOWN_GRACE": "5s",
	}))
	p.waitReady(tb)
	return p
}

func (e *env) launch(tb testing.TB, p *proc, bin string, environ []string) *proc {
	var err error
	p.logPath = filepath.Join(e.logDir, p.name+".log")
	if p.logFile, err = os.Create(p.logPath); err != nil {
		tb.Fatalf("log file: %v", err)
	}
	p.cmd = exec.Command(bin)
	p.cmd.Env = environ
	p.cmd.Stdout, p.cmd.Stderr = p.logFile, p.logFile
	if err := p.cmd.Start(); err != nil {
		tb.Fatalf("start %s: %v", p.name, err)
	}
	p.exited = make(chan error, 1)
	go func() { p.exited <- p.cmd.Wait() }()
	e.procMu.Lock()
	e.procs = append(e.procs, p)
	e.procMu.Unlock()
	return p
}

// waitReady waits for /readyz 200 on the ops listener.
func (p *proc) waitReady(tb testing.TB) {
	deadline := time.Now().Add(readyTimeout)
	for {
		select {
		case err := <-p.exited:
			p.exited <- err
			tb.Fatalf("%s exited before it was ready: %v", p.name, err)
		default:
		}
		resp, err := http.Get("http://" + p.opsAddr + "/readyz")
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		if time.Now().After(deadline) {
			tb.Fatalf("%s: /readyz not 200 within %s", p.name, readyTimeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// signal sends sig to the process.
func (p *proc) signal(sig syscall.Signal) { _ = p.cmd.Process.Signal(sig) }

// stop ends the process: SIGTERM, then SIGKILL after 40 s (the drain budget is 30 s).
func (p *proc) stop() {
	select {
	case err := <-p.exited:
		p.exited <- err
	default:
		p.signal(syscall.SIGTERM)
		select {
		case err := <-p.exited:
			p.exited <- err
		case <-time.After(40 * time.Second):
			_ = p.cmd.Process.Kill()
			<-p.exited
		}
	}
	_ = p.logFile.Close()
}

// stop ends every subprocess and closes the clients.
func (e *env) stop() {
	e.procMu.Lock()
	procs := append([]*proc(nil), e.procs...)
	e.procMu.Unlock()
	var wg sync.WaitGroup
	for _, p := range procs {
		wg.Add(1)
		go func() { defer wg.Done(); p.stop() }()
	}
	wg.Wait()
}

// dumpLogs prints the log of every subprocess (on a failed run).
func (e *env) dumpLogs(w io.Writer) {
	if e == nil {
		return
	}
	e.procMu.Lock()
	defer e.procMu.Unlock()
	for _, p := range e.procs {
		b, err := os.ReadFile(p.logPath)
		if err != nil {
			continue
		}
		_, _ = fmt.Fprintf(w, "\n===== %s log =====\n%s\n", p.name, b)
	}
}
