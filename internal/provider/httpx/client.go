// Package httpx holds the HTTP/WebSocket helpers shared by vendor adapters: the client
// factory, error classification and an SSE reader. It imports provider, never the reverse,
// and no adapter, fake or pricing package.
package httpx

import (
	"net"
	"net/http"
	"time"
)

// NewClient returns an http.Client tuned for streaming vendor APIs: HTTP/2 where offered, a
// large idle pool per host, and no overall or response-header timeout (streams are bounded by
// the caller's context).
func NewClient() *http.Client {
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConnsPerHost:   256,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   3 * time.Second,
		ExpectContinueTimeout: time.Second,
		ResponseHeaderTimeout: 0,
	}
	return &http.Client{Transport: tr}
}
