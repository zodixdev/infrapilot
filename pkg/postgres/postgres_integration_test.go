//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"strings"
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

	"github.com/zodixdev/infrapilot/pkg/health"
	"github.com/zodixdev/infrapilot/pkg/postgres"
	"github.com/zodixdev/infrapilot/pkg/testutil/pgtest"
)

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
			require.NoError(t, postgres.RawPool(pool).QueryRow(ctx, `SELECT NULLIF(current_setting('app.org_id', true), '')`).Scan(&after))
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
		require.True(t, batch.BypassesRLS())
		require.False(t, pool.BypassesRLS())
		require.ErrorIs(t, batch.WithTx(ctx, orgA, func(context.Context, pgx.Tx) error { return nil }), postgres.ErrBypassRLS,
			"tenant context is meaningless on a BYPASSRLS role; WithTx must refuse")
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
	require.NoError(t, postgres.RawPool(pool).QueryRow(t.Context(), `SELECT current_setting('application_name'), current_setting('statement_timeout')`).Scan(&appName, &stmtTimeout))
	require.Equal(t, "pkg-test", appName)
	require.Equal(t, "300ms", stmtTimeout)

	// otelpgx records query spans only under a recording parent span (no orphan DB spans).
	spanCtx, span := tp.Tracer("test").Start(t.Context(), "parent")
	_, err = postgres.RawPool(pool).Exec(spanCtx, `SELECT pg_sleep(2)`)
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

// TestWithSystemTx_IgnoresSessionLevelTenant_Integration reproduces the data-review H2 leak: code that
// bypasses the tenant-safe API leaves a session-level app.org_id on a pooled connection. System
// transactions must still see no tenant rows, and a tenant transaction must see only its own.
func TestWithSystemTx_IgnoresSessionLevelTenant_Integration(t *testing.T) {
	db := pgtest.Start(t)
	migrate(t, db)
	ctx := t.Context()
	pool, err := postgres.Open(ctx, postgres.Config{URL: db.AppURL, MaxConns: 1}) // one connection: reuse is certain
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, pool.Shutdown(context.WithoutCancel(t.Context()))) })

	require.NoError(t, pool.WithTx(ctx, orgA, func(ctx context.Context, tx pgx.Tx) error { return insert(ctx, tx, orgA, "a") }))
	require.NoError(t, pool.WithTx(ctx, orgB, func(ctx context.Context, tx pgx.Tx) error { return insert(ctx, tx, orgB, "b") }))

	_, err = postgres.RawPool(pool).Exec(ctx, `SET app.org_id = '`+orgA.String()+`'`)
	require.NoError(t, err)

	var seen []string
	require.NoError(t, pool.WithSystemTx(ctx, func(ctx context.Context, tx pgx.Tx) (err error) { seen, err = bodies(ctx, tx); return err }))
	require.Empty(t, seen, "system transaction inherited a session-level tenant")

	require.NoError(t, pool.WithTx(ctx, orgB, func(ctx context.Context, tx pgx.Tx) (err error) { seen, err = bodies(ctx, tx); return err }))
	require.Equal(t, []string{"b"}, seen)
}

// TestRoleDefaults_ServerSideTimeouts_Integration: the role carries its own guards (ADR-0018), so a
// session that does not send startup parameters (PgBouncer, psql) is still bounded.
func TestRoleDefaults_ServerSideTimeouts_Integration(t *testing.T) {
	db := pgtest.Start(t)
	conn, err := pgx.Connect(t.Context(), db.AppURL)
	require.NoError(t, err)
	defer func() { _ = conn.Close(context.WithoutCancel(t.Context())) }()
	var stmt, lock, idle string
	require.NoError(t, conn.QueryRow(t.Context(), `SELECT current_setting('statement_timeout'), current_setting('lock_timeout'),
		current_setting('idle_in_transaction_session_timeout')`).Scan(&stmt, &lock, &idle))
	require.Equal(t, []string{"30s", "5s", "1min"}, []string{stmt, lock, idle})

	// The app role cannot connect to other databases (PUBLIC CONNECT revoked).
	other, err := pgx.Connect(t.Context(), db.SuperuserURL)
	require.NoError(t, err)
	defer func() { _ = other.Close(context.WithoutCancel(t.Context())) }()
	var canConnect bool
	require.NoError(t, other.QueryRow(t.Context(), `SELECT has_database_privilege($1, 'postgres', 'CONNECT')`, pgtest.AppRole).Scan(&canConnect))
	require.False(t, canConnect)
}

func TestMigrationConfig_DisablesStatementTimeout_Integration(t *testing.T) {
	db := pgtest.Start(t)
	pool, err := postgres.Open(t.Context(), postgres.MigrationConfig(postgres.Config{URL: db.MigratorURL}))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, pool.Shutdown(context.WithoutCancel(t.Context()))) })
	var stmt, lock string
	require.NoError(t, postgres.RawPool(pool).QueryRow(t.Context(), `SELECT current_setting('statement_timeout'), current_setting('lock_timeout')`).Scan(&stmt, &lock))
	require.Equal(t, "0", stmt)
	require.Equal(t, "3s", lock)
}

func TestTenantSchemaRules_Integration(t *testing.T) {
	db := pgtest.Start(t)
	migrate(t, db)
	pgtest.AssertTenantSchema(t, db.MigratorURL, pgtest.AppRole, nil, nil)

	ctx := t.Context()
	conn, err := pgx.Connect(ctx, db.MigratorURL)
	require.NoError(t, err)
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	for _, s := range []string{
		// H1: a view owned by the migrator would bypass RLS.
		`CREATE VIEW all_notes AS SELECT * FROM notes`,
		// M7: a tenant-blind unique index reveals other tenants' values through duplicate-key errors.
		`ALTER TABLE notes ADD COLUMN slug TEXT`,
		`CREATE UNIQUE INDEX notes_slug_key ON notes (slug)`,
		`CREATE TABLE unscoped (id int PRIMARY KEY)`,
		`CREATE FUNCTION peek() RETURNS bigint LANGUAGE sql SECURITY DEFINER AS 'SELECT count(*) FROM notes'`,
	} {
		_, execErr := conn.Exec(ctx, s)
		require.NoError(t, execErr, s)
	}
	v, err := pgtest.TenantSchemaViolations(ctx, conn, pgtest.AppRole, nil, nil)
	require.NoError(t, err)
	joined := strings.Join(v, "\n")
	require.Contains(t, joined, `view "all_notes" must be created WITH (security_invoker = true)`)
	require.Contains(t, joined, `unique index "notes_slug_key"`)
	require.Contains(t, joined, `table "unscoped" has no organization_id`)
	require.Contains(t, joined, `SECURITY DEFINER function "peek"`)

	// The compliant forms pass.
	for _, s := range []string{
		`DROP VIEW all_notes`, `CREATE VIEW all_notes WITH (security_invoker = true) AS SELECT * FROM notes`,
		`DROP INDEX notes_slug_key`, `CREATE UNIQUE INDEX notes_org_slug_key ON notes (organization_id, slug)`,
	} {
		_, execErr := conn.Exec(ctx, s)
		require.NoError(t, execErr, s)
	}
	v, err = pgtest.TenantSchemaViolations(ctx, conn, pgtest.AppRole, []string{"unscoped"}, []string{"peek"})
	require.NoError(t, err)
	require.Empty(t, v)
}

// TestHealthCheck_SaturatedPoolIsBusyNotDown: with every connection in use the probe times out acquiring,
// which must not mark the replica unready (go-review minor 16).
func TestHealthCheck_SaturatedPoolIsBusyNotDown_Integration(t *testing.T) {
	db := pgtest.Start(t)
	pool, err := postgres.Open(t.Context(), postgres.Config{URL: db.AppURL, MaxConns: 1})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, pool.Shutdown(context.WithoutCancel(t.Context()))) })

	hold, release := make(chan struct{}), make(chan struct{})
	go func() {
		_ = pool.WithSystemTx(context.WithoutCancel(t.Context()), func(context.Context, pgx.Tx) error {
			close(hold)
			<-release
			return nil
		})
	}()
	<-hold
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	require.NoError(t, pool.HealthCheck()(ctx))
	close(release)
}
