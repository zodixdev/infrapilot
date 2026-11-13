package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/zodixdev/infrapilot/pkg/postgres"
	"github.com/zodixdev/infrapilot/services/control-plane/internal/store/db"
)

// PurgeBatchSize bounds the rows one purge statement deletes, so each transaction holds locks briefly.
const PurgeBatchSize = 1000

type purgeStep struct {
	table string
	run   func(q *db.Queries, ctx context.Context, lim int32) (int64, error)
}

// globalSteps remove expired rows of live organizations. Users run last: a user becomes purgeable only once
// no membership references them.
var (
	beforeOrgSteps = []purgeStep{
		{"idempotency_keys", (*db.Queries).PurgeIdempotencyKeys},
		{"audit_events", (*db.Queries).PurgeAuditEvents},
		{"resources", (*db.Queries).PurgeResources},
		{"api_keys", (*db.Queries).PurgeAPIKeys},
		{"projects", (*db.Queries).PurgeProjects},
	}
	afterOrgSteps = []purgeStep{
		{"users", (*db.Queries).PurgeUsers},
	}
)

// orgSteps empty one expired organization, children before parents, each in bounded batches.
var orgSteps = []struct {
	table string
	run   func(q *db.Queries, ctx context.Context, org uuid.UUID, lim int32) (int64, error)
}{
	{"resources", func(q *db.Queries, ctx context.Context, org uuid.UUID, lim int32) (int64, error) {
		return q.PurgeOrganizationResources(ctx, db.PurgeOrganizationResourcesParams{Org: org, Lim: lim})
	}},
	{"api_keys", func(q *db.Queries, ctx context.Context, org uuid.UUID, lim int32) (int64, error) {
		return q.PurgeOrganizationAPIKeys(ctx, db.PurgeOrganizationAPIKeysParams{Org: org, Lim: lim})
	}},
	{"projects", func(q *db.Queries, ctx context.Context, org uuid.UUID, lim int32) (int64, error) {
		return q.PurgeOrganizationProjects(ctx, db.PurgeOrganizationProjectsParams{Org: org, Lim: lim})
	}},
	{"memberships", func(q *db.Queries, ctx context.Context, org uuid.UUID, lim int32) (int64, error) {
		return q.PurgeOrganizationMemberships(ctx, db.PurgeOrganizationMembershipsParams{Org: org, Lim: lim})
	}},
	{"idempotency_keys", func(q *db.Queries, ctx context.Context, org uuid.UUID, lim int32) (int64, error) {
		return q.PurgeOrganizationIdempotencyKeys(ctx, db.PurgeOrganizationIdempotencyKeysParams{Org: org, Lim: lim})
	}},
}

// ErrPurgeNeedsBatchRole is returned when Purge is given a pool that is subject to RLS: it would see no tenant
// rows and silently delete nothing.
var ErrPurgeNeedsBatchRole = errors.New("store: purge must run as the <svc>_batch role (BYPASSRLS)")

// Purge applies the retention rules of ADR-0027 and returns the rows deleted per table. Every statement deletes
// at most PurgeBatchSize rows and commits on its own; an interrupted run resumes on the next schedule. report is
// called after every batch (metrics) and may be nil.
func Purge(ctx context.Context, pool *postgres.Pool, report func(table string, rows int64)) (map[string]int64, error) {
	if !pool.BypassesRLS() {
		return nil, ErrPurgeNeedsBatchRole
	}
	p := purger{pool: pool, deleted: map[string]int64{}, report: report}
	for _, step := range beforeOrgSteps {
		if err := p.drain(ctx, step.table, func(q *db.Queries, ctx context.Context) (int64, error) {
			return step.run(q, ctx, PurgeBatchSize)
		}); err != nil {
			return p.deleted, err
		}
	}
	if err := p.organizations(ctx); err != nil {
		return p.deleted, err
	}
	for _, step := range afterOrgSteps {
		if err := p.drain(ctx, step.table, func(q *db.Queries, ctx context.Context) (int64, error) {
			n, err := step.run(q, ctx, PurgeBatchSize)
			// A membership inserted concurrently can still reference a candidate; that user is simply kept.
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23503" {
				return 0, nil
			}
			return n, err
		}); err != nil {
			return p.deleted, err
		}
	}
	return p.deleted, nil
}

type purger struct {
	pool    *postgres.Pool
	deleted map[string]int64
	report  func(string, int64)
}

func (p *purger) batch(ctx context.Context, table string, fn func(q *db.Queries, ctx context.Context) (int64, error)) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, fmt.Errorf("store: purge %s: %w", table, err)
	}
	var n int64
	err := p.pool.WithSystemTx(ctx, func(ctx context.Context, tx pgx.Tx) (err error) {
		n, err = fn(db.New(tx), ctx)
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("store: purge %s: %w", table, err)
	}
	p.deleted[table] += n
	if p.report != nil {
		p.report(table, n)
	}
	return n, nil
}

// drain repeats fn until a batch deletes fewer than PurgeBatchSize rows.
func (p *purger) drain(ctx context.Context, table string, fn func(q *db.Queries, ctx context.Context) (int64, error)) error {
	for {
		n, err := p.batch(ctx, table, fn)
		if err != nil {
			return err
		}
		if n < PurgeBatchSize {
			return nil
		}
	}
}

func (p *purger) organizations(ctx context.Context) error {
	for {
		var orgs []uuid.UUID
		err := p.pool.WithSystemTx(ctx, func(ctx context.Context, tx pgx.Tx) (err error) {
			orgs, err = db.New(tx).ListExpiredOrganizations(ctx, 100)
			return err
		}, postgres.ReadOnly())
		if err != nil {
			return fmt.Errorf("store: purge organizations: %w", err)
		}
		for _, org := range orgs {
			for _, step := range orgSteps {
				if err := p.drain(ctx, step.table, func(q *db.Queries, ctx context.Context) (int64, error) {
					return step.run(q, ctx, org, PurgeBatchSize)
				}); err != nil {
					return err
				}
			}
			if _, err := p.batch(ctx, "organizations", func(q *db.Queries, ctx context.Context) (int64, error) {
				return q.DeleteOrganization(ctx, org)
			}); err != nil {
				return err
			}
		}
		if len(orgs) < 100 {
			return nil
		}
	}
}
