package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nabePi/kotoki-backend/cmd/internal/providerwire"
	"github.com/nabePi/kotoki-backend/internal/content"
	"github.com/nabePi/kotoki-backend/internal/evaluation"
	"github.com/nabePi/kotoki-backend/internal/evaluation/golden"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/postgres"
	"github.com/nabePi/kotoki-backend/internal/provider"
)

// errExit carries an exit code the subcommand already reported on stderr.
type errExit int

func (e errExit) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

// goldenCaps are the capabilities the golden run calls: EVALUATION, which follows LLM (same vendor).
// providerwire.Assemble fills the other router slots with fake stand-ins.
var goldenCaps = []provider.Capability{provider.CapLLM}

// cmdEvalGolden is `kotoki-cli eval-golden`: the calibration run of spec 10.12. The logic lives in
// golden.Main; this only wires the evaluator: the published EVALUATION prompt version read from
// DATABASE_URL (DB_URL as a fallback) and the evaluation provider chosen by PROVIDER_MODE.
func (a *app) cmdEvalGolden(ctx context.Context, args []string) error {
	code := golden.MainContext(ctx, args, a.out, a.errw, golden.MainDeps{Build: a.buildGoldenEvaluator})
	if code == 0 {
		return nil
	}
	return errExit(code)
}

func (a *app) buildGoldenEvaluator(ctx context.Context) (golden.Evaluator, func(), error) {
	url := a.getenv("DATABASE_URL")
	if url == "" {
		url = a.getenv("DB_URL")
	}
	if url == "" {
		return nil, nil, errors.New("DATABASE_URL (or DB_URL) is required: the published EVALUATION prompt version is read from the database")
	}
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, nil, fmt.Errorf("database: %w", err)
	}
	pv, err := postgres.NewContentRepo(pool).GetPublishedPromptVersion(ctx, content.PromptEvaluation)
	if err != nil {
		pool.Close()
		return nil, nil, fmt.Errorf("published EVALUATION prompt version: %w", err)
	}
	// the prompt version is all the database is needed for
	pool.Close()

	rc, fc, err := providerwire.ParseConfigs()
	if err != nil {
		return nil, nil, err
	}
	asm, err := providerwire.Assemble(rc, fc, goldenCaps, providerwire.Deps{Role: provider.RoleWorker, Clock: clock.System{}})
	if err != nil {
		return nil, nil, err
	}
	router, err := provider.NewRouter(asm.Config, asm.Deps)
	if err != nil {
		return nil, nil, err
	}
	return golden.NewLLMEvaluator(pv, router.Evaluation(), evaluation.NewSchemaCache(), evaluation.DefaultConfig().MinFindingConfidence), nil, nil
}
