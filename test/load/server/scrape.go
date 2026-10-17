// Package server collects the server-side half of a gate run: Prometheus text scrapes (local runs),
// the Prometheus HTTP API (staging) and their conversion into a gate.Observation. A metric that was
// not observed is left out of the Observation, never reported as 0, so gate.Evaluate fails it with
// "no data" instead of passing an empty run.
package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/nabePi/kotoki-backend/test/load/promtext"
)

// Source yields one snapshot of server metrics.
type Source interface {
	Snapshot(ctx context.Context) (promtext.Samples, error)
}

// Scraper scrapes the Prometheus text endpoints of every URL and concatenates the series. Series
// are not labelled with their origin: two pods exposing the same label set add up, which is what
// the sums and bucket quantiles want. Any URL that fails (transport, status, parse) fails the whole
// snapshot; a silently missing pod would make a gate pass on partial data.
type Scraper struct {
	URLs   []string
	Client *http.Client // nil: http.DefaultClient with a 10 s timeout
}

// maxScrapeBytes bounds one response; a /metrics page is well under this.
const maxScrapeBytes = 64 << 20

// Snapshot implements Source.
func (s Scraper) Snapshot(ctx context.Context) (promtext.Samples, error) {
	if len(s.URLs) == 0 {
		return nil, errors.New("server: scraper has no URLs")
	}
	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	var all promtext.Samples
	for _, u := range s.URLs {
		ss, err := scrapeOne(ctx, client, u)
		if err != nil {
			return nil, fmt.Errorf("server: scrape %s: %w", u, err)
		}
		all = append(all, ss...)
	}
	return all, nil
}

func scrapeOne(ctx context.Context, c *http.Client, url string) (promtext.Samples, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	return promtext.Parse(io.LimitReader(resp.Body, maxScrapeBytes))
}
