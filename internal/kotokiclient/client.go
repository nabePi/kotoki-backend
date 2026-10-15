// Package kotokiclient is the test client of the realtime backend (spec §14.11.1): a REST client
// with cookie-based refresh, a WebSocket v1 client, and (with kotokiclient/audio) the audio and
// pacing a scripted conversation needs. It is the only consumer of the wire protocol outside the
// server; it imports conversation/protocol and conversation/audio only.
package kotokiclient

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
)

const (
	apiPrefix      = "/api/v1"
	defaultTimeout = 30 * time.Second
	maxBody        = 4 << 20
	// errBodyPreview bounds how much of a non-JSON error body lands in APIError.Message.
	errBodyPreview = 200
)

// APIError is a REST error response (spec 5.8.2, D-013).
type APIError struct {
	Status    int
	Code      string
	Message   string
	RequestID string
	Details   json.RawMessage
}

// Error implements error.
func (e *APIError) Error() string {
	return fmt.Sprintf("kotokiclient: %d %s: %s (request_id %s)", e.Status, e.Code, e.Message, e.RequestID)
}

// Client is the REST client. It keeps the access token in memory and the refresh cookie in a
// cookie jar. Safe for concurrent use.
type Client struct {
	base  *url.URL
	hc    *http.Client
	clk   clock.Clock
	ids   idgen.Generator
	mu    sync.RWMutex
	token string
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient replaces the HTTP client. It should carry a cookie jar for Refresh to work.
func WithHTTPClient(hc *http.Client) Option { return func(c *Client) { c.hc = hc } }

// WithClock replaces the clock used for event timestamps and receive times.
func WithClock(clk clock.Clock) Option { return func(c *Client) { c.clk = clk } }

// WithIDs replaces the generator of Idempotency-Key and event ids.
func WithIDs(g idgen.Generator) Option { return func(c *Client) { c.ids = g } }

// New returns a client for the API at baseURL (scheme and host, e.g. http://localhost:8080; the
// /api/v1 prefix is added). A malformed baseURL makes every call fail with that error.
func New(baseURL string, opts ...Option) *Client {
	c := &Client{clk: clock.System{}, ids: idgen.NewV7()}
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || u.Host == "" {
		u = &url.URL{} // reported by do()
	}
	c.base = u
	jar, _ := cookiejar.New(nil) // never fails with nil options
	c.hc = &http.Client{Jar: jar, Timeout: defaultTimeout}
	for _, o := range opts {
		o(c)
	}
	return c
}

// AccessToken returns the current access token ("" before Register/Login/Refresh).
func (c *Client) AccessToken() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.token
}

// SetAccessToken installs an access token obtained elsewhere (e.g. stored credentials).
func (c *Client) SetAccessToken(t string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.token = t
}

type authResult struct {
	AccessToken string `json:"access_token"`
	User        User   `json:"user"`
}

type callOpts struct {
	body    any
	headers map[string]string
	noAuth  bool
	query   url.Values
}

// do performs one JSON request. A non-2xx answer becomes *APIError. out may be nil.
func (c *Client) do(ctx context.Context, method, path string, o callOpts, out any) error {
	if c.base.Host == "" {
		return errors.New("kotokiclient: invalid base URL")
	}
	u := *c.base
	u.Path = strings.TrimRight(c.base.Path, "/") + apiPrefix + path
	if len(o.query) > 0 {
		u.RawQuery = o.query.Encode()
	}
	var body io.Reader
	if o.body != nil {
		b, err := json.Marshal(o.body)
		if err != nil {
			return fmt.Errorf("kotokiclient: encode request: %w", err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return fmt.Errorf("kotokiclient: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if o.body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if !o.noAuth {
		if t := c.AccessToken(); t != "" {
			req.Header.Set("Authorization", "Bearer "+t)
		}
	}
	for k, v := range o.headers {
		req.Header.Set(k, v)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("kotokiclient: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return fmt.Errorf("kotokiclient: read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return decodeAPIError(resp.StatusCode, raw)
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("kotokiclient: decode %s %s: %w", method, path, err)
	}
	return nil
}

func decodeAPIError(status int, raw []byte) *APIError {
	var env struct {
		Error *struct {
			Code      string          `json:"code"`
			Message   string          `json:"message"`
			RequestID string          `json:"request_id"`
			Details   json.RawMessage `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || env.Error == nil {
		msg := strings.TrimSpace(string(raw))
		if len(msg) > errBodyPreview {
			msg = msg[:errBodyPreview]
		}
		return &APIError{Status: status, Message: msg}
	}
	e := env.Error
	return &APIError{Status: status, Code: e.Code, Message: e.Message, RequestID: e.RequestID, Details: e.Details}
}

// Register creates an account and signs in (the access token is kept, the refresh cookie lands
// in the jar).
func (c *Client) Register(ctx context.Context, r RegisterRequest) (User, error) {
	var res authResult
	if err := c.do(ctx, http.MethodPost, "/auth/register", callOpts{body: r, noAuth: true}, &res); err != nil {
		return User{}, err
	}
	c.SetAccessToken(res.AccessToken)
	return res.User, nil
}

// Login signs in; the access token is kept and the refresh cookie lands in the jar.
func (c *Client) Login(ctx context.Context, email, password string) error {
	var res authResult
	body := struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}{email, password}
	if err := c.do(ctx, http.MethodPost, "/auth/login", callOpts{body: body, noAuth: true}, &res); err != nil {
		return err
	}
	c.SetAccessToken(res.AccessToken)
	return nil
}

// Refresh exchanges the refresh cookie for a new access token (the cookie rotates).
func (c *Client) Refresh(ctx context.Context) error {
	var res authResult
	if err := c.do(ctx, http.MethodPost, "/auth/refresh", callOpts{noAuth: true}, &res); err != nil {
		return err
	}
	c.SetAccessToken(res.AccessToken)
	return nil
}

type items[T any] struct {
	Items []T `json:"items"`
}

// Catalog lists the active topics, scenarios and characters.
func (c *Client) Catalog(ctx context.Context) (Catalog, error) {
	var (
		cat Catalog
		t   items[Topic]
		s   items[Scenario]
		ch  items[Character]
	)
	if err := c.do(ctx, http.MethodGet, "/topics", callOpts{}, &t); err != nil {
		return Catalog{}, err
	}
	if err := c.do(ctx, http.MethodGet, "/scenarios", callOpts{}, &s); err != nil {
		return Catalog{}, err
	}
	if err := c.do(ctx, http.MethodGet, "/characters", callOpts{}, &ch); err != nil {
		return Catalog{}, err
	}
	cat.Topics, cat.Scenarios, cat.Characters = t.Items, s.Items, ch.Items
	return cat, nil
}

// CreateSession reserves a session. The Idempotency-Key header is r.IdempotencyKey, or a fresh
// UUID when empty, so a retry with the same key returns the same session.
func (c *Client) CreateSession(ctx context.Context, r CreateSessionRequest) (Session, error) {
	key := r.IdempotencyKey
	if key == "" {
		key = c.ids.New().String()
	}
	var s Session
	err := c.do(ctx, http.MethodPost, "/sessions", callOpts{body: r, headers: map[string]string{"Idempotency-Key": key}}, &s)
	return s, err
}

// CancelSession cancels a RESERVED session.
func (c *Client) CancelSession(ctx context.Context, id uuid.UUID) error {
	return c.do(ctx, http.MethodPost, "/sessions/"+id.String()+"/cancel", callOpts{}, nil)
}

// RealtimeTicket issues a one-time WebSocket ticket for the session.
func (c *Client) RealtimeTicket(ctx context.Context, sessionID uuid.UUID) (string, error) {
	var res struct {
		Ticket string `json:"ticket"`
	}
	if err := c.do(ctx, http.MethodPost, "/sessions/"+sessionID.String()+"/realtime-ticket", callOpts{}, &res); err != nil {
		return "", err
	}
	return res.Ticket, nil
}

// GetSession reads one session.
func (c *Client) GetSession(ctx context.Context, id uuid.UUID) (Session, error) {
	var s Session
	err := c.do(ctx, http.MethodGet, "/sessions/"+id.String(), callOpts{}, &s)
	return s, err
}

// GetEvaluation reads the session evaluation (served from M7 on).
func (c *Client) GetEvaluation(ctx context.Context, sessionID uuid.UUID) (Evaluation, error) {
	var e Evaluation
	err := c.do(ctx, http.MethodGet, "/sessions/"+sessionID.String()+"/evaluation", callOpts{}, &e)
	return e, err
}

// Balance reads the caller's balance.
func (c *Client) Balance(ctx context.Context) (Balance, error) {
	var b Balance
	err := c.do(ctx, http.MethodGet, "/balance", callOpts{}, &b)
	return b, err
}

// Turns reads one page of the transcript: the turns after sequence number afterSeq (nil = from
// the start), at most limit (0 = server default). The string is the server's next_cursor, ""
// when there is no further page; to continue, pass the last turn's SequenceNumber as afterSeq.
func (c *Client) Turns(ctx context.Context, id uuid.UUID, afterSeq *int, limit int) ([]Turn, string, error) {
	q := url.Values{}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if afterSeq != nil {
		q.Set("cursor", turnsCursor(*afterSeq))
	}
	var page struct {
		Data       []Turn  `json:"data"`
		NextCursor *string `json:"next_cursor"`
	}
	if err := c.do(ctx, http.MethodGet, "/sessions/"+id.String()+"/turns", callOpts{query: q}, &page); err != nil {
		return nil, "", err
	}
	next := ""
	if page.NextCursor != nil {
		next = *page.NextCursor
	}
	return page.Data, next, nil
}

// turnsCursor builds the opaque list cursor {"v":1,"k":seq} (base64url, no padding) the server
// hands out as next_cursor (httpapi.EncodeCursor).
func turnsCursor(seq int) string {
	b, _ := json.Marshal(struct {
		V int `json:"v"`
		K int `json:"k"`
	}{1, seq})
	return base64.RawURLEncoding.EncodeToString(b)
}
