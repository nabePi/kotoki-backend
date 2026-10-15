package testutil_test

import (
	"context"
	"path/filepath"
	"testing"

	"go.opentelemetry.io/otel"

	"github.com/nabePi/kotoki-backend/internal/testutil"
)

func TestTestdataPathFindsRoot(t *testing.T) {
	p := testutil.TestdataPath(t, "api/rest/.gitkeep")
	if !filepath.IsAbs(p) {
		t.Fatalf("not absolute: %s", p)
	}
}

func TestNewTraceRecorderCapturesSpan(t *testing.T) {
	rec := testutil.NewTraceRecorder(t)
	_, span := otel.Tracer("x").Start(context.Background(), "a")
	span.End()
	if got := rec.Find("a").Name; got != "a" {
		t.Fatalf("got %q", got)
	}
	if len(rec.Spans()) != 1 {
		t.Fatalf("spans=%d", len(rec.Spans()))
	}
}
