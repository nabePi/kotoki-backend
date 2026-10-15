package postgres

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/nabePi/kotoki-backend/internal/platform/metrics"
	"github.com/nabePi/kotoki-backend/internal/platform/otel"
)

var nameRE = regexp.MustCompile(`^-- name: (\w+)`)

// queryName returns the sqlc query name from a leading `-- name: X` comment,
// or "query" for anything else. It is the only part of a statement that ever
// reaches spans and metrics (SQL text is never recorded).
func queryName(sql string) string {
	if m := nameRE.FindStringSubmatch(sql); m != nil {
		return m[1]
	}
	return "query"
}

type queryKey struct{}

type queryState struct {
	span  trace.Span
	name  string
	start time.Time
}

// queryTracer is a pgx.QueryTracer producing one span and one histogram
// observation per query.
type queryTracer struct{ metrics *metrics.DB }

var _ pgx.QueryTracer = (*queryTracer)(nil)

func (t *queryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	name := queryName(data.SQL)
	ctx, span := otel.Start(ctx, "db."+name,
		attribute.String("db.system", "postgresql"),
		attribute.String("db.operation.name", name),
	)
	return context.WithValue(ctx, queryKey{}, &queryState{span: span, name: name, start: time.Now()})
}

func (t *queryTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	st, ok := ctx.Value(queryKey{}).(*queryState)
	if !ok {
		return
	}
	if t.metrics != nil {
		t.metrics.ObserveQuery(st.name, time.Since(st.start))
	}
	err := spanError(data.Err)
	otel.End(st.span, &err)
}

// spanError hides integrity-constraint violations (class 23) from the span: repositories
// translate them into domain errors, so exporting them as exceptions skews error dashboards.
func spanError(err error) error {
	if strings.HasPrefix(pgCode(err), "23") {
		return nil
	}
	return err
}
