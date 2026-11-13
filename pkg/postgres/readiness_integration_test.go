//go:build integration

package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"

	"github.com/zodixdev/infrapilot/pkg/app"
	"github.com/zodixdev/infrapilot/pkg/health"
	"github.com/zodixdev/infrapilot/pkg/httpserver"
	"github.com/zodixdev/infrapilot/pkg/postgres"
	"github.com/zodixdev/infrapilot/pkg/testutil/pgtest"
)

type readyzResponse struct {
	code   int
	body   string
	report health.Report
}

func getReadyz(ctx context.Context, u string) (readyzResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, http.NoBody)
	if err != nil {
		return readyzResponse{}, err
	}
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		return readyzResponse{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return readyzResponse{}, err
	}
	r := readyzResponse{code: resp.StatusCode, body: string(b)}
	if err := json.Unmarshal(b, &r.report); err != nil {
		return readyzResponse{}, err
	}
	return r, nil
}

// TestReadyz_FlipsOnPostgresLossThroughApp_Integration wires the pool, the health registry, the HTTP
// server and the App exactly as a service's cmd/ does and proves, over the real /readyz endpoint, that
// readiness flips to 503 while Postgres is unreachable, that the reported error carries no credentials,
// that readiness recovers, and that the whole stack (including the pool closer) shuts down cleanly.
func TestReadyz_FlipsOnPostgresLossThroughApp_Integration(t *testing.T) {
	db := pgtest.Start(t)
	appURL, err := url.Parse(db.AppURL)
	require.NoError(t, err)
	password, ok := appURL.User.Password()
	require.True(t, ok)

	pool, err := postgres.Open(t.Context(), postgres.Config{URL: db.AppURL, MaxConns: 2, ConnectTimeout: 2 * time.Second})
	require.NoError(t, err)

	reg := health.NewRegistry(health.WithTimeout(500 * time.Millisecond))
	reg.Register("postgres", pool.HealthCheck(), health.Periodic(100*time.Millisecond))
	mux := http.NewServeMux()
	mux.Handle("GET /readyz", reg.ReadinessHandler())
	srv, err := httpserver.New(t.Context(), httpserver.Config{
		Addr: "127.0.0.1:0", ReadHeaderTimeout: time.Second, ReadTimeout: 5 * time.Second,
		WriteTimeout: 10 * time.Second, IdleTimeout: 5 * time.Second,
	}, mux)
	require.NoError(t, err)

	a, err := app.New(app.Config{ShutdownTimeout: 5 * time.Second}, app.WithReadinessGate(reg), app.WithSignals())
	require.NoError(t, err)
	a.Add(
		app.Closer("postgres", pool.Shutdown),
		app.Func("health", reg.Run, nil),
		srv,
	)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()

	readyz := "http://" + srv.Addr().String() + "/readyz"
	poll := func(want int) readyzResponse {
		t.Helper()
		var last readyzResponse
		require.Eventually(t, func() bool {
			r, err := getReadyz(t.Context(), readyz)
			if err != nil {
				return false
			}
			last = r
			return r.code == want
		}, 20*time.Second, 25*time.Millisecond, "/readyz never answered %d (last: %d %s)", want, last.code, last.body)
		return last
	}

	up := poll(http.StatusOK)
	require.Equal(t, health.StatusUp, up.report.Checks["postgres"].Status)

	db.Pause(t)
	down := poll(http.StatusServiceUnavailable)
	require.Equal(t, health.StatusDown, down.report.Status)
	require.Equal(t, health.StatusDown, down.report.Checks["postgres"].Status, "the dependency check is what failed: %s", down.body)
	require.NotEmpty(t, down.report.Checks["postgres"].Error)
	require.NotContains(t, down.body, password, "/readyz must not leak the database password")

	db.Unpause(t)
	recovered := poll(http.StatusOK)
	require.Equal(t, health.StatusUp, recovered.report.Checks["postgres"].Status)

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(15 * time.Second):
		t.Fatal("app did not shut down")
	}
	require.Equal(t, int32(0), pool.Stat().TotalConns(), "pool closed by the App")
}

// TestWithTx_RollsBackWhenCommitFails_Integration: a deferred constraint fails at COMMIT, after fn returned
// nil. WithTx must report the commit error and nothing written in the transaction may persist.
func TestWithTx_RollsBackWhenCommitFails_Integration(t *testing.T) {
	db := pgtest.Start(t)
	migrate(t, db)
	ctx := t.Context()
	conn, err := pgx.Connect(ctx, db.MigratorURL)
	require.NoError(t, err)
	_, err = conn.Exec(ctx, `ALTER TABLE notes ADD CONSTRAINT notes_org_body_key UNIQUE (organization_id, body) DEFERRABLE INITIALLY DEFERRED`)
	require.NoError(t, err)
	require.NoError(t, conn.Close(ctx))

	pool, err := postgres.Open(ctx, postgres.Config{URL: db.AppURL, MaxConns: 1})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, pool.Shutdown(context.WithoutCancel(t.Context()))) })

	err = pool.WithTx(ctx, orgA, func(ctx context.Context, tx pgx.Tx) error {
		// Both inserts succeed inside the transaction; the duplicate is detected only at COMMIT.
		return errors.Join(insert(ctx, tx, orgA, "marker"), insert(ctx, tx, orgA, "dup"), insert(ctx, tx, orgA, "dup"))
	})
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	require.Equal(t, "23505", pgErr.Code, "unique_violation at commit: %v", err)
	require.ErrorContains(t, err, "postgres: commit")
	assertNotPresent(ctx, t, pool, orgA, "marker")
	assertNotPresent(ctx, t, pool, orgA, "dup")
}

// TestWithTx_RollsBackWhenContextCanceled_Integration: the caller's context is cancelled mid-transaction
// (client went away). The rollback must still run (it is detached from the cancelled context), nothing
// persists, and the single pooled connection is usable afterwards without a leaked tenant context.
func TestWithTx_RollsBackWhenContextCanceled_Integration(t *testing.T) {
	db := pgtest.Start(t)
	migrate(t, db)
	pool, err := postgres.Open(t.Context(), postgres.Config{URL: db.AppURL, MaxConns: 1})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, pool.Shutdown(context.WithoutCancel(t.Context()))) })

	reqCtx, cancel := context.WithCancel(t.Context())
	err = pool.WithTx(reqCtx, orgA, func(ctx context.Context, tx pgx.Tx) error {
		if insErr := insert(ctx, tx, orgA, "cancelled"); insErr != nil {
			return insErr
		}
		cancel()
		return ctx.Err()
	})
	require.ErrorIs(t, err, context.Canceled)

	ctx := t.Context()
	assertNotPresent(ctx, t, pool, orgA, "cancelled")
	var leaked *string
	require.NoError(t, pool.WithSystemTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT NULLIF(current_setting('app.org_id', true), '')`).Scan(&leaked)
	}))
	require.Nil(t, leaked)
	require.NoError(t, pool.HealthCheck()(ctx))
}
