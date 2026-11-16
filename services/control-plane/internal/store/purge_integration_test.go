//go:build integration

package store_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"

	"github.com/zodixdev/infrapilot/services/control-plane/internal/store"
	"github.com/zodixdev/infrapilot/services/control-plane/internal/store/db"
	"github.com/zodixdev/infrapilot/services/control-plane/internal/testdb"
)

func TestPurge_RechecksExpiredOrganization_Integration(t *testing.T) {
	d := testdb.Start(t)
	ctx := t.Context()
	tenant := seedTenant(t, d, "restored")
	migrator, err := pgx.Connect(ctx, d.MigratorURL)
	require.NoError(t, err)
	defer func() { _ = migrator.Close(context.WithoutCancel(ctx)) }()
	_, err = migrator.Exec(ctx, `UPDATE organizations SET deleted_at = now() - interval '31 days' WHERE id = $1`, tenant.org)
	require.NoError(t, err)
	var expiredIDs []uuid.UUID
	require.NoError(t, d.Batch.WithSystemTx(ctx, func(ctx context.Context, tx pgx.Tx) (err error) {
		expiredIDs, err = db.New(tx).ListExpiredOrganizations(ctx, 100)
		return err
	}))
	require.Equal(t, []uuid.UUID{tenant.org}, expiredIDs)
	_, err = migrator.Exec(ctx, `UPDATE organizations SET deleted_at = NULL WHERE id = $1`, tenant.org)
	require.NoError(t, err)
	var stillExpired bool
	require.NoError(t, d.Batch.WithSystemTx(ctx, func(ctx context.Context, tx pgx.Tx) (err error) {
		stillExpired, err = db.New(tx).LockExpiredOrganization(ctx, expiredIDs[0])
		return err
	}))
	require.False(t, stillExpired)
	err = d.App.WithTx(ctx, tenant.org, func(ctx context.Context, tx pgx.Tx) error {
		_, lockErr := db.New(tx).LockExpiredOrganization(ctx, tenant.org)
		return lockErr
	})
	var denied *pgconn.PgError
	require.ErrorAs(t, err, &denied)
	require.Equal(t, "42501", denied.Code)
}

func TestPurge_Retention_Integration(t *testing.T) {
	d := testdb.Start(t)
	ctx := t.Context()
	a, b := seedTenant(t, d, "alpha"), seedTenant(t, d, "beta")

	// A second resource in alpha, deleted recently: inside the 30-day window, must survive.
	fresh := uuid.Must(uuid.NewV7())
	require.NoError(t, d.App.WithTx(ctx, a.org, func(ctx context.Context, tx pgx.Tx) error {
		_, err := db.New(tx).InsertResource(ctx, db.InsertResourceParams{ID: fresh, OrganizationID: a.org, ProjectID: a.project, Kind: "service", Name: "api", Labels: []byte(`{}`)})
		return err
	}))
	// An abandoned user (no membership, not seen for 401 days) and a recently seen one without memberships.
	var abandoned, recent uuid.UUID
	require.NoError(t, d.App.WithSystemTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := db.New(tx)
		u1, err := q.UpsertUser(ctx, db.UpsertUserParams{ID: uuid.Must(uuid.NewV7()), Subject: "gone", Email: "gone@example.com"})
		if err != nil {
			return err
		}
		u2, err := q.UpsertUser(ctx, db.UpsertUserParams{ID: uuid.Must(uuid.NewV7()), Subject: "new", Email: "new@example.com"})
		abandoned, recent = u1.ID, u2.ID
		return err
	}))

	// Age rows as the superuser (timestamps are server-set in normal operation).
	su, err := pgx.Connect(ctx, d.SuperuserURL)
	require.NoError(t, err)
	defer func() { _ = su.Close(context.WithoutCancel(ctx)) }()
	for _, s := range []struct {
		sql  string
		args []any
	}{
		{`UPDATE organizations SET deleted_at = now() - interval '31 days' WHERE id = $1`, []any{b.org}},
		{`UPDATE resources SET deleted_at = now() - interval '31 days' WHERE id = $1`, []any{a.resource}},
		{`UPDATE resources SET deleted_at = now() - interval '1 day' WHERE id = $1`, []any{fresh}},
		{`UPDATE api_keys SET revoked_at = now() - interval '31 days' WHERE id = $1`, []any{a.key}},
		{`UPDATE audit_events SET created_at = now() - interval '401 days' WHERE organization_id = $1`, []any{a.org}},
		{`UPDATE idempotency_keys SET created_at = now() - interval '25 hours' WHERE organization_id = $1`, []any{a.org}},
		{`UPDATE users SET last_seen_at = now() - interval '401 days' WHERE id = $1`, []any{abandoned}},
	} {
		_, execErr := su.Exec(ctx, s.sql, s.args...)
		require.NoError(t, execErr, s.sql)
	}

	// An organization-create idempotency record past its 24 h window.
	_, err = su.Exec(ctx, `INSERT INTO org_create_keys (principal_id, key, organization_id, request_hash, created_at)
		VALUES ($1, 'k', $2, decode(repeat('ab', 32), 'hex'), now() - interval '25 hours')`, a.user, a.org)
	require.NoError(t, err)

	_, err = store.Purge(ctx, d.App, nil)
	require.ErrorIs(t, err, store.ErrPurgeNeedsBatchRole, "under RLS the purge would silently delete nothing")

	reported := map[string]int64{}
	deleted, err := store.Purge(ctx, d.Batch, func(table string, n int64) { reported[table] += n })
	require.NoError(t, err)
	require.Equal(t, deleted, reported)
	// alpha's expired rows plus everything beta owned.
	require.EqualValues(t, 1, deleted["organizations"])
	require.EqualValues(t, 2, deleted["resources"])
	require.EqualValues(t, 2, deleted["api_keys"])
	require.EqualValues(t, 1, deleted["projects"])
	require.EqualValues(t, 1, deleted["memberships"])
	require.EqualValues(t, 1, deleted["audit_events"])
	require.EqualValues(t, 2, deleted["idempotency_keys"])
	require.EqualValues(t, 1, deleted["users"])
	require.EqualValues(t, 1, deleted["org_create_keys"])

	exists := func(table string, where string, args ...any) bool {
		var ok bool
		require.NoError(t, su.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM `+table+` `+where+`)`, args...).Scan(&ok))
		return ok
	}
	for _, table := range tenantTables {
		if table == "audit_events" {
			continue
		}
		require.False(t, exists(table, "WHERE organization_id = $1", b.org), "purged org's %s must be gone", table)
	}
	require.True(t, exists("audit_events", "WHERE organization_id = $1", b.org), "audit history outlives the organization (ADR-0027)")
	require.False(t, exists("api_key_index", "WHERE organization_id = $1", b.org))
	require.False(t, exists("api_key_index", "WHERE key_id = $1", a.key), "lookup row follows its key")
	require.True(t, exists("resources", "WHERE id = $1", fresh), "rows inside the grace period stay")
	require.True(t, exists("users", "WHERE id = $1", recent))
	require.True(t, exists("users", "WHERE id = $1", b.user), "users of a purged org stay until inactive for 400 days")
	require.True(t, exists("projects", "WHERE id = $1", a.project))

	again, err := store.Purge(ctx, d.Batch, nil)
	require.NoError(t, err)
	for table, n := range again {
		require.Zero(t, n, "second run deletes nothing from %s", table)
	}
}
