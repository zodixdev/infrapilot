package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/zodixdev/infrapilot/pkg/postgres"
	"github.com/zodixdev/infrapilot/services/control-plane/internal/store/db"
)

// PurgeBatchSize bounds the rows one purge statement deletes, so each transaction holds locks briefly.
const PurgeBatchSize = 1000

// purgeSteps run in dependency order: tenants' own expired rows first, organizations (cascading to everything
// they own) next, and users last, because a user becomes purgeable only once no membership references them.
var purgeSteps = []struct {
	table string
	run   func(q *db.Queries, ctx context.Context, lim int32) (int64, error)
}{
	{"idempotency_keys", (*db.Queries).PurgeIdempotencyKeys},
	{"audit_events", (*db.Queries).PurgeAuditEvents},
	{"resources", (*db.Queries).PurgeResources},
	{"api_keys", (*db.Queries).PurgeAPIKeys},
	{"projects", (*db.Queries).PurgeProjects},
	{"organizations", (*db.Queries).PurgeOrganizations},
	{"users", (*db.Queries).PurgeUsers},
}

// ErrPurgeNeedsBatchRole is returned when Purge is given a pool that is subject to RLS: it would see no tenant
// rows and silently delete nothing.
var ErrPurgeNeedsBatchRole = errors.New("store: purge must run as the <svc>_batch role (BYPASSRLS)")

// Purge applies the retention rules of ADR-0027 and returns the rows deleted per table. Each batch commits on
// its own; an interrupted run resumes where it stopped on the next schedule. report is called after every
// batch (metrics), may be nil.
func Purge(ctx context.Context, pool *postgres.Pool, report func(table string, rows int64)) (map[string]int64, error) {
	if !pool.BypassesRLS() {
		return nil, ErrPurgeNeedsBatchRole
	}
	deleted := make(map[string]int64, len(purgeSteps))
	for _, step := range purgeSteps {
		for {
			if err := ctx.Err(); err != nil {
				return deleted, fmt.Errorf("store: purge %s: %w", step.table, err)
			}
			var n int64
			err := pool.WithSystemTx(ctx, func(ctx context.Context, tx pgx.Tx) (err error) {
				n, err = step.run(db.New(tx), ctx, PurgeBatchSize)
				return err
			})
			if err != nil {
				return deleted, fmt.Errorf("store: purge %s: %w", step.table, err)
			}
			deleted[step.table] += n
			if report != nil {
				report(step.table, n)
			}
			if n < PurgeBatchSize {
				break
			}
		}
	}
	return deleted, nil
}
