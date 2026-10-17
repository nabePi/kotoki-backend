package live

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nabePi/kotoki-backend/test/load/verify"
)

// RunDB is verify.Run over a new pgx pool on dsn.
func RunDB(ctx context.Context, dsn string, o verify.Options) ([]verify.Violation, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()
	return verify.Run(ctx, pool, o)
}
