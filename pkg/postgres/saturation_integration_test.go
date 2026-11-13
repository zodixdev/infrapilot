//go:build integration

package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/zodixdev/infrapilot/pkg/postgres"
	"github.com/zodixdev/infrapilot/pkg/testutil/pgtest"
)

// TestHealthCheck_SaturatedPoolAndDeadDatabaseIsDown_Integration: with every connection held by hung requests
// and Postgres unresponsive, the probe must not mistake "dead" for "busy".
func TestHealthCheck_SaturatedPoolAndDeadDatabaseIsDown_Integration(t *testing.T) {
	db := pgtest.Start(t)
	pool, err := postgres.Open(t.Context(), postgres.Config{URL: db.AppURL, MaxConns: 1, ConnectTimeout: time.Second})
	require.NoError(t, err)
	t.Cleanup(func() { pool.Close() })

	hold := make(chan struct{})
	go func() {
		_ = pool.WithSystemTx(context.WithoutCancel(t.Context()), func(ctx context.Context, tx pgx.Tx) error {
			close(hold)
			_, err := tx.Exec(ctx, `SELECT pg_sleep(5)`)
			return err
		})
	}()
	<-hold
	db.Pause(t)
	t.Cleanup(func() { db.Unpause(t) })

	for range 3 {
		ctx, cancel := context.WithTimeout(t.Context(), 600*time.Millisecond)
		err := pool.HealthCheck()(ctx)
		cancel()
		require.Error(t, err, "a saturated pool in front of a dead database is down")
		require.Contains(t, err.Error(), "direct probe failed")
	}
}
