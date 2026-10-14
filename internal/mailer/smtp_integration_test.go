//go:build integration

package mailer

import (
	"bufio"
	"context"
	"crypto/tls"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/auth"
)

// plainSMTPServer accepts one connection, never advertises STARTTLS and records
// whether the client ever got to DATA.
func plainSMTPServer(t *testing.T) (host string, port int, gotData *atomic.Bool) {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	gotData = &atomic.Bool{}
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		w := bufio.NewWriter(c)
		reply := func(s string) { _, _ = w.WriteString(s + "\r\n"); _ = w.Flush() }
		reply("220 localhost ESMTP")
		r := bufio.NewReader(c)
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			switch cmd := strings.ToUpper(strings.TrimSpace(line)); {
			case strings.HasPrefix(cmd, "EHLO"):
				reply("250-localhost")
				reply("250 8BITMIME")
			case strings.HasPrefix(cmd, "DATA"):
				gotData.Store(true)
				reply("354 go ahead")
			case strings.HasPrefix(cmd, "QUIT"):
				reply("221 bye")
				return
			default:
				reply("250 OK")
			}
		}
	}()
	h, p, _ := net.SplitHostPort(ln.Addr().String())
	port, _ = strconv.Atoi(p)
	return h, port, gotData
}

func TestSMTPRequiresSTARTTLS(t *testing.T) {
	host, port, gotData := plainSMTPServer(t)
	m := NewSMTP(Config{From: "Kotoki <no-reply@kotoki.id>", SMTPHost: host, SMTPPort: port, SMTPUsername: "u", SMTPPassword: "p"}, &tls.Config{MinVersion: tls.VersionTLS12})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err := m.Send(ctx, auth.MailMessage{To: "hana@example.com", Subject: "x", TextBody: "secret token"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "STARTTLS")
	time.Sleep(100 * time.Millisecond)
	require.False(t, gotData.Load(), "no body must be sent without TLS")
}
