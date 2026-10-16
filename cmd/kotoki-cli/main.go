// Command kotoki-cli is the official test client of the Kotoki backend (spec §14.11): REST
// subcommands plus `converse`, which plays WAV files into a realtime session and records what
// comes back. The logic lives in internal/kotokiclient; this package parses flags, keeps the
// stored credentials and prints. Conversation text is printed locally only.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/signal"
	"time"

	"github.com/nabePi/kotoki-backend/internal/kotokiclient"
)

// Defaults of the global flags (§14.11.2).
const (
	defaultAPIURL  = "http://localhost:8080"
	defaultWSURL   = "ws://localhost:8081/api/v1/realtime"
	defaultOrigin  = "http://localhost:3000"
	defaultTimeout = 30 * time.Second
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr, lookupEnv)
	stop()
	os.Exit(code)
}

func lookupEnv(key string) string {
	v, _ := os.LookupEnv(key)
	return v
}

// globals are the flags every subcommand accepts (also from the environment).
type globals struct {
	APIURL  string
	WSURL   string
	Origin  string
	JSON    bool
	Timeout time.Duration
}

// defaultGlobals reads the KOTOKI_CLI_* environment on top of the built-in defaults.
func defaultGlobals(getenv func(string) string) globals {
	pick := func(key, def string) string {
		if v := getenv(key); v != "" {
			return v
		}
		return def
	}
	return globals{
		APIURL:  pick("KOTOKI_CLI_API_URL", defaultAPIURL),
		WSURL:   pick("KOTOKI_CLI_WS_URL", defaultWSURL),
		Origin:  pick("KOTOKI_CLI_ORIGIN", defaultOrigin),
		Timeout: defaultTimeout,
	}
}

// bind registers the global flags on fs; the current values are the defaults, so a flag given
// before the subcommand carries over to the subcommand's own flag set.
func (g *globals) bind(fs *flag.FlagSet) {
	fs.StringVar(&g.APIURL, "api-url", g.APIURL, "REST API base URL (env KOTOKI_CLI_API_URL)")
	fs.StringVar(&g.WSURL, "ws-url", g.WSURL, "realtime WebSocket URL (env KOTOKI_CLI_WS_URL)")
	fs.StringVar(&g.Origin, "origin", g.Origin, "Origin header of the WebSocket handshake (env KOTOKI_CLI_ORIGIN)")
	fs.BoolVar(&g.JSON, "json", g.JSON, "print JSON lines instead of text")
	fs.DurationVar(&g.Timeout, "timeout", g.Timeout, "timeout per REST request")
}

// errUsage marks a mistake in the command line: exit code 2.
type errUsage struct{ msg string }

func (e *errUsage) Error() string { return e.msg }

func usagef(format string, args ...any) error { return &errUsage{msg: fmt.Sprintf(format, args...)} }

// errPrinted means the flag package already reported the problem.
var errPrinted = errors.New("flag error already printed")

type app struct {
	out    io.Writer
	errw   io.Writer
	getenv func(string) string
	g      globals
}

// run executes one command line and returns the process exit code: 0 success, 1 failure,
// 2 usage error.
func run(ctx context.Context, args []string, out, errw io.Writer, getenv func(string) string) int {
	a := &app{out: out, errw: errw, getenv: getenv, g: defaultGlobals(getenv)}
	fs := flag.NewFlagSet("kotoki-cli", flag.ContinueOnError)
	fs.SetOutput(errw)
	fs.Usage = a.usage
	a.g.bind(fs)
	err := fs.Parse(args)
	if err == nil {
		err = a.dispatch(ctx, fs.Args())
	} else {
		err = wrapParse(err)
	}
	return a.exitCode(err)
}

func (a *app) exitCode(err error) int {
	var u *errUsage
	var ex errExit
	switch {
	case err == nil, errors.Is(err, flag.ErrHelp):
		return 0
	case errors.Is(err, errPrinted):
		return 2
	case errors.As(err, &ex):
		return int(ex)
	case errors.As(err, &u):
		_, _ = fmt.Fprintf(a.errw, "kotoki-cli: %s\n", u.msg)
		return 2
	default:
		_, _ = fmt.Fprintf(a.errw, "kotoki-cli: %v\n", err)
		return 1
	}
}

func wrapParse(err error) error {
	if errors.Is(err, flag.ErrHelp) {
		return err
	}
	return errPrinted
}

func (a *app) dispatch(ctx context.Context, rest []string) error {
	if len(rest) == 0 {
		a.usage()
		return usagef("missing subcommand")
	}
	cmd, args := rest[0], rest[1:]
	switch cmd {
	case "help":
		a.usage()
		return nil
	case "register":
		return a.cmdRegister(ctx, args)
	case "login":
		return a.cmdLogin(ctx, args)
	case "catalog":
		return a.cmdCatalog(ctx, args)
	case "balance":
		return a.cmdBalance(ctx, args)
	case "session":
		return a.cmdSession(ctx, args)
	case "ticket":
		return a.cmdTicket(ctx, args)
	case "converse", "chat":
		return a.cmdConverse(ctx, args)
	case "evaluation":
		return a.cmdEvaluation(ctx, args)
	case "eval-golden":
		return a.cmdEvalGolden(ctx, args)
	case "provider-bench":
		return a.cmdProviderBench(ctx, args)
	default:
		a.usage()
		return usagef("unknown subcommand %q", cmd)
	}
}

func (a *app) usage() {
	_, _ = fmt.Fprint(a.errw, `kotoki-cli: test client of the Kotoki backend

usage: kotoki-cli [global flags] <subcommand> [flags]

subcommands:
  register    --email --password [--display-name] [--terms-version]
  login       --email --password
  catalog     list active characters and scenarios
  balance     show the balance
  session     create | get | cancel | end | transcript
  ticket      --session   print a realtime ticket (debug)
  converse    (alias chat) play --wav files into a realtime session
  evaluation  get --session [--wait]
  eval-golden --dataset [--runs 3] [--out report.json] [--baseline] [--export-findings] [--ratings]
              calibrate the evaluation rubric on a golden set (DATABASE_URL, PROVIDER_MODE)
  provider-bench --suite stt|llm|llm-json|tts|e2e|concurrency|all --out DIR [--dataset testdata/benchmark]
              [--models-file bench/models.yaml] [--repeat 3] [--audio-root DIR] [--human CSV] [--concurrency-steps 10,50,100,200]
              benchmark the provider candidates against the spec 8.10.4 gates (PROVIDER_MODE, vendor keys)

global flags (also from the environment):
  --api-url   REST base URL            (KOTOKI_CLI_API_URL, default `+defaultAPIURL+`)
  --ws-url    realtime WebSocket URL   (KOTOKI_CLI_WS_URL,  default `+defaultWSURL+`)
  --origin    WebSocket Origin         (KOTOKI_CLI_ORIGIN,  default `+defaultOrigin+`)
  --json      JSON lines output
  --timeout   timeout per REST request (default 30s)

credentials: $XDG_CONFIG_HOME/kotoki-cli/credentials.json, one entry per --api-url.
`)
}

// newFlagSet returns a flag set for a subcommand with the global flags bound.
func (a *app) newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet("kotoki-cli "+name, flag.ContinueOnError)
	fs.SetOutput(a.errw)
	a.g.bind(fs)
	return fs
}

// parse parses a subcommand's flags; positional arguments are a usage error.
func (a *app) parse(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		return wrapParse(err)
	}
	if fs.NArg() > 0 {
		return usagef("%s: unexpected argument %q", fs.Name(), fs.Arg(0))
	}
	return nil
}

// printJSON writes v as one JSON line.
func (a *app) printJSON(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(a.out, "%s\n", b)
	return err
}

func (a *app) printf(format string, args ...any) {
	_, _ = fmt.Fprintf(a.out, format, args...)
}

// newHTTPClient is the REST transport: a cookie jar (refresh cookie) and the --timeout.
func (a *app) newHTTPClient() *http.Client {
	jar, _ := cookiejar.New(nil) // never fails with nil options
	return &http.Client{Jar: jar, Timeout: a.g.Timeout}
}

// cliClient is a REST client wired to the stored credentials of one api-url.
type cliClient struct {
	*kotokiclient.Client
	store  credStore
	key    string
	loaded serverCreds
}

// open builds the client for a.g.APIURL with the stored tokens, if any.
func (a *app) open() (*cliClient, error) {
	store, err := newCredStore(a.getenv)
	if err != nil {
		return nil, err
	}
	loaded, err := store.load(a.g.APIURL)
	if err != nil {
		return nil, err
	}
	c := kotokiclient.New(a.g.APIURL, kotokiclient.WithHTTPClient(a.newHTTPClient()))
	c.SetAccessToken(loaded.AccessToken)
	if loaded.RefreshToken != "" {
		c.SetRefreshToken(loaded.RefreshToken)
	}
	return &cliClient{Client: c, store: store, key: a.g.APIURL, loaded: loaded}, nil
}

// persist stores the tokens when they changed since open (a Refresh rotates both).
func (cc *cliClient) persist(email string) error {
	cur := serverCreds{AccessToken: cc.AccessToken(), RefreshToken: cc.RefreshToken(), Email: cc.loaded.Email}
	if email != "" {
		cur.Email = email
	}
	if cur == cc.loaded {
		return nil
	}
	if err := cc.store.save(cc.key, cur); err != nil {
		return err
	}
	cc.loaded = cur
	return nil
}

// call runs fn; on a 401 it refreshes the access token once with the stored refresh token and
// retries. The new tokens are persisted either way.
func (cc *cliClient) call(ctx context.Context, fn func() error) error {
	err := fn()
	var ae *kotokiclient.APIError
	if err == nil || !errors.As(err, &ae) || ae.Status != http.StatusUnauthorized || cc.RefreshToken() == "" {
		return err
	}
	if rerr := cc.Refresh(ctx); rerr != nil {
		return fmt.Errorf("%w (refreshing the access token failed: %w; run `kotoki-cli login`)", err, rerr)
	}
	if perr := cc.persist(""); perr != nil {
		return perr
	}
	return fn()
}

// run is call followed by persist: whatever the outcome, rotated tokens are kept.
func (cc *cliClient) run(ctx context.Context, fn func() error) error {
	err := cc.call(ctx, fn)
	if perr := cc.persist(""); err == nil {
		err = perr
	}
	return err
}
