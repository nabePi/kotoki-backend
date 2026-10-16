//go:build integration || e2e

package testutil

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	amqp091 "github.com/rabbitmq/amqp091-go"
	tcrabbit "github.com/testcontainers/testcontainers-go/modules/rabbitmq"
)

const rabbitImage = "rabbitmq:4-management"

var rb struct {
	once     sync.Once
	amqpURL  string // server root, vhost "/"
	httpURL  string // management API root
	user     string
	password string
	err      error
}

func startRabbit() {
	if v := os.Getenv("TEST_RABBITMQ_URL"); v != "" {
		u, err := url.Parse(v)
		if err != nil {
			rb.err = fmt.Errorf("TEST_RABBITMQ_URL: %w", err)
			return
		}
		rb.amqpURL = v
		rb.user = u.User.Username()
		rb.password, _ = u.User.Password()
		rb.httpURL = os.Getenv("TEST_RABBITMQ_MGMT_URL")
		if rb.httpURL == "" {
			rb.httpURL = "http://" + net.JoinHostPort(u.Hostname(), "15672")
		}
		return
	}
	ctx := context.Background()
	c, err := tcrabbit.Run(ctx, rabbitImage)
	if err != nil {
		rb.err = fmt.Errorf("start rabbitmq: %w", err)
		return
	}
	if rb.amqpURL, rb.err = c.AmqpURL(ctx); rb.err != nil {
		return
	}
	if rb.httpURL, rb.err = c.HttpURL(ctx); rb.err != nil {
		return
	}
	rb.user, rb.password = "guest", "guest"
}

// NewRabbitMQ returns the AMQP URL of a fresh virtual host (t_<uuid>) on a shared RabbitMQ
// (quorum queues, management plugin). The vhost is deleted on cleanup. TEST_RABBITMQ_URL
// (and optionally TEST_RABBITMQ_MGMT_URL) overrides the container.
func NewRabbitMQ(t testing.TB) string {
	t.Helper()
	rb.once.Do(startRabbit)
	if rb.err != nil {
		t.Fatalf("testutil: rabbitmq: %v", rb.err)
	}
	vhost := "t_" + uuid.NewString()
	adm := &RabbitAdmin{t: t, vhost: vhost}
	// the management API can lag a moment behind the AMQP port at first start
	var err error
	for i := 0; i < 30; i++ {
		if _, err = adm.do(http.MethodPut, "/api/vhosts/"+url.PathEscape(vhost), nil); err == nil {
			break
		}
		time.Sleep(time.Second)
	}
	if err != nil {
		t.Fatalf("testutil: rabbitmq: create vhost: %v", err)
	}
	perm := map[string]string{"configure": ".*", "write": ".*", "read": ".*"}
	if _, err := adm.do(http.MethodPut, "/api/permissions/"+url.PathEscape(vhost)+"/"+url.PathEscape(rb.user), perm); err != nil {
		t.Fatalf("testutil: rabbitmq: grant vhost: %v", err)
	}
	t.Cleanup(func() {
		_, _ = adm.do(http.MethodDelete, "/api/vhosts/"+url.PathEscape(vhost), nil)
	})
	u, _ := url.Parse(rb.amqpURL)
	u.Path = "/" + vhost
	u.RawPath = ""
	return u.String()
}

// RabbitAdmin inspects and disturbs one vhost through the management API and AMQP.
type RabbitAdmin struct {
	t       testing.TB
	vhost   string
	amqpURL string
}

// RabbitMQAdmin binds an admin to the vhost of url (as returned by NewRabbitMQ).
func RabbitMQAdmin(t testing.TB, amqpURL string) *RabbitAdmin {
	t.Helper()
	u, err := url.Parse(amqpURL)
	if err != nil {
		t.Fatalf("testutil: rabbitmq admin: %v", err)
	}
	vhost := strings.TrimPrefix(u.Path, "/")
	if vhost == "" {
		vhost = "/"
	}
	return &RabbitAdmin{t: t, vhost: vhost, amqpURL: amqpURL}
}

func (a *RabbitAdmin) do(method, path string, body any) ([]byte, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, rb.httpURL+path, rd)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(rb.user, rb.password)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return out, fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, out)
	}
	return out, nil
}

type definitions struct {
	Queues []struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	} `json:"queues"`
	Exchanges []struct {
		Name string `json:"name"`
	} `json:"exchanges"`
}

// definitions comes from the metadata store, so it is current immediately (the queue list
// endpoints lag by the statistics interval).
func (a *RabbitAdmin) definitions() definitions {
	a.t.Helper()
	b, err := a.do(http.MethodGet, "/api/definitions/"+url.PathEscape(a.vhost), nil)
	if err != nil {
		a.t.Fatalf("testutil: rabbitmq definitions: %v", err)
	}
	var d definitions
	if err := json.Unmarshal(b, &d); err != nil {
		a.t.Fatalf("testutil: rabbitmq definitions: %v", err)
	}
	return d
}

// Queues returns the queue names of the vhost, sorted.
func (a *RabbitAdmin) Queues() []string {
	a.t.Helper()
	var out []string
	for _, q := range a.definitions().Queues {
		out = append(out, q.Name)
	}
	sort.Strings(out)
	return out
}

// Exchanges returns the user-declared exchange names (no default, no amq.*), sorted.
func (a *RabbitAdmin) Exchanges() []string {
	a.t.Helper()
	var out []string
	for _, e := range a.definitions().Exchanges {
		if e.Name == "" || strings.HasPrefix(e.Name, "amq.") {
			continue
		}
		out = append(out, e.Name)
	}
	sort.Strings(out)
	return out
}

// QueueArgs returns the declaration arguments of queue (JSON numbers decode as float64).
func (a *RabbitAdmin) QueueArgs(queue string) map[string]any {
	a.t.Helper()
	for _, q := range a.definitions().Queues {
		if q.Name == queue {
			return q.Arguments
		}
	}
	a.t.Fatalf("testutil: rabbitmq: queue %q not found", queue)
	return nil
}

// QueueMessages returns the number of ready messages in queue, read live over AMQP (a passive
// declare) because the management statistics lag by seconds.
func (a *RabbitAdmin) QueueMessages(queue string) int {
	a.t.Helper()
	conn, err := amqp091.Dial(a.amqpURL)
	if err != nil {
		a.t.Fatalf("testutil: rabbitmq: dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	ch, err := conn.Channel()
	if err != nil {
		a.t.Fatalf("testutil: rabbitmq: channel: %v", err)
	}
	q, err := ch.QueueDeclarePassive(queue, true, false, false, false, nil)
	if err != nil {
		a.t.Fatalf("testutil: rabbitmq: queue %q: %v", queue, err)
	}
	return q.Messages
}

// CloseAllConnections force-closes every client connection on the vhost.
func (a *RabbitAdmin) CloseAllConnections() {
	a.t.Helper()
	// connection listing lags too; poll until at least one is seen, then close them all
	deadline := time.Now().Add(15 * time.Second)
	for {
		b, err := a.do(http.MethodGet, "/api/vhosts/"+url.PathEscape(a.vhost)+"/connections", nil)
		if err != nil {
			a.t.Fatalf("testutil: rabbitmq: list connections: %v", err)
		}
		var conns []struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(b, &conns); err != nil {
			a.t.Fatalf("testutil: rabbitmq: list connections: %v", err)
		}
		if len(conns) > 0 {
			for _, c := range conns {
				_, _ = a.do(http.MethodDelete, "/api/connections/"+url.PathEscape(c.Name), nil)
			}
			return
		}
		if time.Now().After(deadline) {
			a.t.Fatalf("testutil: rabbitmq: no connection to close")
		}
		time.Sleep(250 * time.Millisecond)
	}
}
