//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.uber.org/goleak"

	"github.com/zodixdev/infrapilot/pkg/health"
	"github.com/zodixdev/infrapilot/pkg/postgres"
	"github.com/zodixdev/infrapilot/pkg/testutil/pgtest"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		// testcontainers keeps a reaper/docker client goroutine for the process lifetime.
		goleak.IgnoreAnyFunction("github.com/testcontainers/testcontainers-go.(*Reaper).connect.func1"),
		goleak.IgnoreTopFunction("internal/poll.runtime_pollWait"),
	)
}

var (
	orgA = uuid.MustParse("0192f7a0-3c1e-7c9a-9d41-9a1f0f3b2c11")
	orgB = uuid.MustParse("0192f7a0-3c1e-7c9a-9d41-9a1f0f3b2c22")
)

// migrate creates a tenant table the way service migrations must (ADR-0018): as the migrator role,
// with RLS enabled, a tenant policy and DML grants to the app role.
func migrate(t *testing.T, db *pgtest.DB) {
	t.Helper()
	ctx := t.Context()
	conn, err := pgx.Connect(ctx, db.MigratorURL)
	require.NoError(t, err)
	defer func() { _ = conn.Close(ctx) }()
	for _, s := range []string{
		`CREATE TABLE notes (id BIGSERIAL PRIMARY KEY, organization_id UUID NOT NULL, body TEXT NOT NULL)`,
		`CREATE INDEX notes_org_idx ON notes (organization_id)`,
		`ALTER TABLE notes ENABLE ROW LEVEL SECURITY`,
		`CREATE POLICY notes_tenant ON notes USING (organization_id = NULLIF(current_setting('app.org_id', true), '')::uuid)
		   WITH CHECK (organization_id = NULLIF(current_setting('app.org_id', true), '')::uuid)`,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON notes TO ` + pgtest.AppRole,
		`GRANT USAGE ON SEQUENCE notes_id_seq TO ` + pgtest.AppRole,
		`GRANT SELECT ON notes TO ` + pgtest.BatchRole,
	} {
		_, err := conn.Exec(ctx, s)
		require.NoError(t, err, s)
	}
}

func openApp(t *testing.T, db *pgtest.DB, opts ...postgres.Option) *postgres.Pool {
	t.Helper()
	pool, err := postgres.Open(t.Context(), postgres.Config{URL: db.AppURL, ApplicationName: "pkg-test", MaxConns: 4}, opts...)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, pool.Shutdown(context.WithoutCancel(t.Context()))) })
	return pool
}

func insert(ctx context.Context, tx pgx.Tx, org uuid.UUID, body string) error {
	_, err := tx.Exec(ctx, `INSERT INTO notes (organization_id, body) VALUES ($1, $2)`, org, body)
	return err
}

func bodies(ctx context.Context, tx pgx.Tx) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT body FROM notes ORDER BY id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

func TestWithTx_Integration(t *testing.T) {
	db := pgtest.Start(t)
	migrate(t, db)
	pool := openApp(t, db)
	ctx := t.Context()

	t.Run("sets app.org_id for the transaction only", func(t *testing.T) {
		var inside string
		require.NoError(t, pool.WithTx(ctx, orgA, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT current_setting('app.org_id', true)`).Scan(&inside)
		}))
		require.Equal(t, orgA.String(), inside)

		// Same pooled connection(s), new statement: the setting is gone.
		for range 8 {
			var after *string
			require.NoError(t, pool.QueryRow(ctx, `SELECT NULLIF(current_setting('app.org_id', true), '')`).Scan(&after))
			require.Nil(t, after, "tenant context leaked out of the transaction")
		}
	})

	t.Run("rejects the nil tenant", func(t *testing.T) {
		err := pool.WithTx(ctx, uuid.Nil, func(context.Context, pgx.Tx) error { return nil })
		require.ErrorIs(t, err, postgres.ErrNoTenant)
	})

	t.Run("commits on success and RLS blocks cross-tenant access", func(t *testing.T) {
		require.NoError(t, pool.WithTx(ctx, orgA, func(ctx context.Context, tx pgx.Tx) error {
			return errors.Join(insert(ctx, tx, orgA, "a1"), insert(ctx, tx, orgA, "a2"))
		}))
		require.NoError(t, pool.WithTx(ctx, orgB, func(ctx context.Context, tx pgx.Tx) error { return insert(ctx, tx, orgB, "b1") }))

		var seenA, seenB []string
		require.NoError(t, pool.WithTx(ctx, orgA, func(ctx context.Context, tx pgx.Tx) (err error) { seenA, err = bodies(ctx, tx); return err }))
		require.NoError(t, pool.WithTx(ctx, orgB, func(ctx context.Context, tx pgx.Tx) (err error) { seenB, err = bodies(ctx, tx); return err }))
		require.Equal(t, []string{"a1", "a2"}, seenA)
		require.Equal(t, []string{"b1"}, seenB)

		// Writing a row for another tenant violates the policy's WITH CHECK.
		err := pool.WithTx(ctx, orgA, func(ctx context.Context, tx pgx.Tx) error { return insert(ctx, tx, orgB, "smuggled") })
		var pgErr *pgconn.PgError
		require.ErrorAs(t, err, &pgErr)
		require.Equal(t, "42501", pgErr.Code, "insufficient_privilege from RLS: %v", err)
	})

	t.Run("system transaction sees no tenant rows under the app role", func(t *testing.T) {
		var seen []string
		require.NoError(t, pool.WithSystemTx(ctx, func(ctx context.Context, tx pgx.Tx) (err error) { seen, err = bodies(ctx, tx); return err }))
		require.Empty(t, seen, "missing tenant context must fail closed")
	})

	t.Run("batch role bypasses RLS explicitly", func(t *testing.T) {
		batch, err := postgres.Open(ctx, postgres.Config{URL: db.BatchURL, ApplicationName: "nightly-report", MaxConns: 1})
		require.NoError(t, err)
		defer func() { require.NoError(t, batch.Shutdown(context.WithoutCancel(ctx))) }()
		var seen []string
		require.NoError(t, batch.WithSystemTx(ctx, func(ctx context.Context, tx pgx.Tx) (err error) { seen, err = bodies(ctx, tx); return err }, postgres.ReadOnly()))
		require.Len(t, seen, 3)
	})

	t.Run("rolls back on error", func(t *testing.T) {
		boom := errors.New("business rule violated")
		err := pool.WithTx(ctx, orgA, func(ctx context.Context, tx pgx.Tx) error {
			if err := insert(ctx, tx, orgA, "rolled-back"); err != nil {
				return err
			}
			return boom
		})
		require.ErrorIs(t, err, boom)
		assertNotPresent(ctx, t, pool, orgA, "rolled-back")
	})

	t.Run("rolls back on panic and re-panics", func(t *testing.T) {
		require.PanicsWithValue(t, "handler bug", func() {
			_ = pool.WithTx(ctx, orgA, func(ctx context.Context, tx pgx.Tx) error {
				if err := insert(ctx, tx, orgA, "panicked"); err != nil {
					return err
				}
				panic("handler bug")
			})
		})
		assertNotPresent(ctx, t, pool, orgA, "panicked")
		// The connection went back to the pool in a usable state.
		require.NoError(t, pool.HealthCheck()(ctx))
	})

	t.Run("read-only transactions reject writes", func(t *testing.T) {
		err := pool.WithTx(ctx, orgA, func(ctx context.Context, tx pgx.Tx) error { return insert(ctx, tx, orgA, "ro") }, postgres.ReadOnly())
		var pgErr *pgconn.PgError
		require.ErrorAs(t, err, &pgErr)
		require.Equal(t, "25006", pgErr.Code, "read_only_sql_transaction")
	})

	t.Run("app role cannot run DDL", func(t *testing.T) {
		err := pool.WithSystemTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `CREATE TABLE escalation (id int)`)
			return err
		})
		var pgErr *pgconn.PgError
		require.ErrorAs(t, err, &pgErr)
		require.Equal(t, "42501", pgErr.Code)
	})
}

func assertNotPresent(ctx context.Context, t *testing.T, pool *postgres.Pool, org uuid.UUID, body string) {
	t.Helper()
	var n int
	require.NoError(t, pool.WithTx(ctx, org, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM notes WHERE body = $1`, body).Scan(&n)
	}))
	require.Zero(t, n)
}

func TestOpen_TimeoutsMetricsAndTracing_Integration(t *testing.T) {
	db := pgtest.Start(t)
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.WithoutCancel(t.Context())) })
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { _ = tp.Shutdown(context.WithoutCancel(t.Context())) })

	pool, err := postgres.Open(t.Context(), postgres.Config{
		URL: db.AppURL, ApplicationName: "pkg-test", MaxConns: 2, StatementTimeout: 300 * time.Millisecond,
	}, postgres.WithMeterProvider(mp), postgres.WithTracerProvider(tp))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, pool.Shutdown(context.WithoutCancel(t.Context()))) })

	var appName, stmtTimeout string
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT current_setting('application_name'), current_setting('statement_timeout')`).Scan(&appName, &stmtTimeout))
	require.Equal(t, "pkg-test", appName)
	require.Equal(t, "300ms", stmtTimeout)

	// otelpgx records query spans only under a recording parent span (no orphan DB spans).
	spanCtx, span := tp.Tracer("test").Start(t.Context(), "parent")
	_, err = pool.Exec(spanCtx, `SELECT pg_sleep(2)`)
	span.End()
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	require.Equal(t, "57014", pgErr.Code, "query_canceled by statement_timeout")

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(t.Context(), &rm))
	names := map[string]bool{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			names[m.Name] = true
		}
	}
	require.True(t, names["pgxpool_total_conns"] && names["pgxpool_acquire_total"] && names["pgxpool_max_conns"], "%v", names)
	require.NotEmpty(t, exporter.GetSpans(), "query spans recorded")
}

func TestReadiness_FlipsOnPostgresLoss_Integration(t *testing.T) {
	db := pgtest.Start(t)
	pool, err := postgres.Open(t.Context(), postgres.Config{URL: db.AppURL, MaxConns: 2, ConnectTimeout: 2 * time.Second})
	require.NoError(t, err)
	t.Cleanup(func() { _ = pool.Shutdown(context.WithoutCancel(t.Context())) })

	reg := health.NewRegistry(health.WithTimeout(500 * time.Millisecond))
	reg.SetReady(true)
	reg.Register("postgres", pool.HealthCheck(), health.Periodic(200*time.Millisecond))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- reg.Run(ctx) }()

	require.Eventually(t, func() bool { return reg.Readiness(t.Context()).Status == health.StatusUp }, 5*time.Second, 50*time.Millisecond)

	db.Pause(t)
	require.Eventually(t, func() bool { return reg.Readiness(t.Context()).Status == health.StatusDown }, 10*time.Second, 50*time.Millisecond,
		"readiness must flip to down while Postgres is unreachable")

	db.Unpause(t)
	require.Eventually(t, func() bool { return reg.Readiness(t.Context()).Status == health.StatusUp }, 20*time.Second, 100*time.Millisecond,
		"readiness must recover once Postgres is back")

	cancel()
	require.NoError(t, <-done)
}
