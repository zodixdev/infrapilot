//go:build integration

package migrations

import (
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
	"golang.org/x/sync/errgroup"

	"github.com/zodixdev/infrapilot/pkg/postgres"
	"github.com/zodixdev/infrapilot/pkg/testutil/pgtest"
)

func TestResourceStateBackfillResumesFromM02_Integration(t *testing.T) {
	baseline := goleak.IgnoreCurrent()
	t.Cleanup(func() { goleak.VerifyNone(t, baseline) })
	ctx := t.Context()
	pg := pgtest.Start(t)
	db, err := postgres.OpenSQL(postgres.MigrationConfig(postgres.Config{URL: pg.MigratorURL}))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	provider, err := goose.NewProvider(goose.DialectPostgres, db, FS)
	require.NoError(t, err)
	_, err = provider.UpTo(ctx, 1)
	require.NoError(t, err)

	org, project := uuid.New(), uuid.New()
	_, err = db.ExecContext(ctx, `INSERT INTO organizations (id, name) VALUES ($1, 'Infrastructure')`, org)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO projects (id, organization_id, name, slug) VALUES ($1, $2, 'Web', 'web')`, project, org)
	require.NoError(t, err)
	count := resourceStateBackfillBatchSize + 2
	_, err = db.ExecContext(ctx, `INSERT INTO resources (id, organization_id, project_id, kind, name, deleted_at)
		SELECT ('00000000-0000-0000-0000-' || lpad(i::text, 12, '0'))::uuid, $1, $2, 'service',
		       'legacy-' || i, CASE WHEN i = $3 THEN now() END
		FROM generate_series(10, $3::integer) AS i`, org, project, count+9)
	require.NoError(t, err)
	_, err = provider.Up(ctx)
	require.NoError(t, err)

	// Commit just one batch, as if the migrate Job stopped before finishing.
	var enrolled int64
	var complete bool
	require.NoError(t, db.QueryRowContext(ctx, backfillResourceStateBatch, resourceStateBackfillBatchSize).Scan(&enrolled, &complete))
	require.EqualValues(t, resourceStateBackfillBatchSize, enrolled)
	require.False(t, complete)
	// The INSERT trigger captures rows below the checkpoint and above the legacy upper bound.
	_, err = db.ExecContext(ctx, `INSERT INTO resources (id, organization_id, project_id, kind, name)
		VALUES ('00000000-0000-0000-0000-000000000001', $1, $2, 'service', 'below-checkpoint'),
		       ('ffffffff-ffff-ffff-ffff-ffffffffffff', $1, $2, 'service', 'above-upper-bound')`, org, project)
	require.NoError(t, err)

	logger := slog.New(slog.DiscardHandler)
	var hooks errgroup.Group
	for range 2 {
		hooks.Go(func() error { return Migrate(ctx, pg.MigratorURL, logger) })
	}
	require.NoError(t, hooks.Wait())
	require.NoError(t, Migrate(ctx, pg.MigratorURL, logger))
	var total, keys, active, inactive int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*), count(DISTINCT routing_key),
		count(*) FILTER (WHERE status = 1), count(*) FILTER (WHERE status = 2)
		FROM resource_state_outbox`).Scan(&total, &keys, &active, &inactive))
	require.Equal(t, count+2, total)
	require.Equal(t, total, keys)
	require.Equal(t, count+1, active)
	require.Equal(t, 1, inactive)
	require.NoError(t, db.QueryRowContext(ctx, `SELECT completed_at IS NOT NULL FROM resource_state_backfill`).Scan(&complete))
	require.True(t, complete)
}
