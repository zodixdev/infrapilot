// Package testdb starts a disposable PostgreSQL with the ADR-0018 roles and the control-plane migrations
// applied, for integration tests of the store, service and transport layers.
package testdb

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zodixdev/infrapilot/pkg/postgres"
	"github.com/zodixdev/infrapilot/pkg/testutil/pgtest"
	"github.com/zodixdev/infrapilot/services/control-plane/migrations"
)

// NonTenantTables are the tables declared non-tenant to AssertTenantSchema: global identities, the pre-tenant
// API key lookup (ADR-0018) and goose's bookkeeping.
var NonTenantTables = []string{"users", "api_key_index", "goose_db_version"}

// DB is a migrated database with pools for the runtime (app) and purge (batch) roles.
type DB struct {
	*pgtest.DB
	App   *postgres.Pool
	Batch *postgres.Pool
}

// Start launches Postgres, applies the migrations as the migrator role and opens both pools.
func Start(t *testing.T) *DB {
	t.Helper()
	pg := pgtest.Start(t)
	require.NoError(t, migrations.Migrate(t.Context(), pg.MigratorURL, slog.New(slog.DiscardHandler)))
	open := func(url, name string) *postgres.Pool {
		p, err := postgres.Open(t.Context(), postgres.Config{URL: url, ApplicationName: name, MaxConns: 8})
		require.NoError(t, err)
		t.Cleanup(func() { _ = p.Shutdown(context.WithoutCancel(t.Context())) })
		return p
	}
	return &DB{DB: pg, App: open(pg.AppURL, "control-plane-test"), Batch: open(pg.BatchURL, "control-plane-purge-test")}
}
