//go:build integration

package postgres_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/zodixdev/infrapilot/pkg/postgres"
	"github.com/zodixdev/infrapilot/pkg/testutil/pgtest"
)

// migrateUserScoped mirrors ADR-0023: a tenant table "orgs" keyed on id, a tenant table "members", tenant
// policies on app.org_id, and SELECT-only user policies on app.user_id.
func migrateUserScoped(t *testing.T, db *pgtest.DB) {
	t.Helper()
	ctx := t.Context()
	conn, err := pgx.Connect(ctx, db.MigratorURL)
	require.NoError(t, err)
	defer func() { _ = conn.Close(ctx) }()
	const org, user = `NULLIF(current_setting('app.org_id', true), '')::uuid`, `NULLIF(current_setting('app.user_id', true), '')::uuid`
	for _, s := range []string{
		`CREATE TABLE orgs (id UUID PRIMARY KEY, organization_id UUID GENERATED ALWAYS AS (id) STORED, name TEXT NOT NULL)`,
		`CREATE TABLE members (organization_id UUID NOT NULL REFERENCES orgs (id), user_id UUID NOT NULL, PRIMARY KEY (organization_id, user_id))`,
		`CREATE INDEX members_user_idx ON members (user_id, organization_id)`,
		`ALTER TABLE orgs ENABLE ROW LEVEL SECURITY`,
		`ALTER TABLE members ENABLE ROW LEVEL SECURITY`,
		`CREATE POLICY orgs_tenant ON orgs USING (id = ` + org + `) WITH CHECK (id = ` + org + `)`,
		`CREATE POLICY members_tenant ON members USING (organization_id = ` + org + `) WITH CHECK (organization_id = ` + org + `)`,
		`CREATE POLICY members_self ON members FOR SELECT USING (user_id = ` + user + `)`,
		`CREATE POLICY orgs_member ON orgs FOR SELECT USING (EXISTS (SELECT 1 FROM members m WHERE m.organization_id = orgs.id AND m.user_id = ` + user + `))`,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON orgs, members TO ` + pgtest.AppRole,
	} {
		_, err := conn.Exec(ctx, s)
		require.NoError(t, err, s)
	}
}

func TestWithUserTx_Integration(t *testing.T) {
	db := pgtest.Start(t)
	migrateUserScoped(t, db)
	pool := openApp(t, db)
	ctx := t.Context()
	alice, bob := uuid.New(), uuid.New()

	seed := func(org uuid.UUID, name string, users ...uuid.UUID) {
		require.NoError(t, pool.WithTx(ctx, org, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `INSERT INTO orgs (id, name) VALUES ($1, $2)`, org, name); err != nil {
				return err
			}
			for _, u := range users {
				if _, err := tx.Exec(ctx, `INSERT INTO members (organization_id, user_id) VALUES ($1, $2)`, org, u); err != nil {
					return err
				}
			}
			return nil
		}))
	}
	seed(orgA, "alpha", alice)
	seed(orgB, "beta", alice, bob)

	type view struct{ orgs, members []string }
	read := func(run func(fn func(ctx context.Context, tx pgx.Tx) error) error) view {
		var v view
		require.NoError(t, run(func(ctx context.Context, tx pgx.Tx) error {
			rows, err := tx.Query(ctx, `SELECT name FROM orgs ORDER BY name`)
			if err != nil {
				return err
			}
			if v.orgs, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
				return err
			}
			rows, err = tx.Query(ctx, `SELECT o.name || ':' || CASE WHEN m.user_id = $1 THEN 'alice' ELSE 'other' END
			                          FROM members m JOIN orgs o ON o.id = m.organization_id ORDER BY 1`, alice)
			if err != nil {
				return err
			}
			v.members, err = pgx.CollectRows(rows, pgx.RowTo[string])
			return err
		}))
		return v
	}
	asUser := func(u uuid.UUID) func(fn func(ctx context.Context, tx pgx.Tx) error) error {
		return func(fn func(ctx context.Context, tx pgx.Tx) error) error { return pool.WithUserTx(ctx, u, fn) }
	}

	t.Run("user sees the orgs they belong to and only their own memberships", func(t *testing.T) {
		require.Equal(t, view{orgs: []string{"alpha", "beta"}, members: []string{"alpha:alice", "beta:alice"}}, read(asUser(alice)))
		require.Equal(t, view{orgs: []string{"beta"}, members: []string{"beta:other"}}, read(asUser(bob)))
		require.Equal(t, view{orgs: []string{}, members: []string{}}, read(asUser(uuid.New())), "a user without memberships sees nothing")
	})

	t.Run("tenant transaction is not widened by the user policy", func(t *testing.T) {
		v := read(func(fn func(ctx context.Context, tx pgx.Tx) error) error { return pool.WithTx(ctx, orgA, fn) })
		require.Equal(t, view{orgs: []string{"alpha"}, members: []string{"alpha:alice"}}, v)
	})

	t.Run("a session-level app.user_id never applies", func(t *testing.T) {
		// Poison every pooled connection with a session-level value, as code outside pkg/postgres might.
		conns := make([]*pgxpool.Conn, 0, 4)
		for range 4 {
			c, err := postgres.RawPool(pool).Acquire(ctx)
			require.NoError(t, err)
			_, err = c.Exec(ctx, `SELECT set_config('app.user_id', $1, false)`, alice.String())
			require.NoError(t, err)
			conns = append(conns, c)
		}
		for _, c := range conns {
			c.Release()
		}
		require.Equal(t, view{orgs: []string{}, members: []string{}}, read(func(fn func(ctx context.Context, tx pgx.Tx) error) error { return pool.WithSystemTx(ctx, fn) }), "system transactions clear the user context")
		v := read(func(fn func(ctx context.Context, tx pgx.Tx) error) error { return pool.WithTx(ctx, orgA, fn) })
		require.Equal(t, []string{"alpha"}, v.orgs, "tenant transactions clear the user context")
	})

	t.Run("user transactions are read-only", func(t *testing.T) {
		err := pool.WithUserTx(ctx, alice, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE orgs SET name = 'hijacked'`)
			return err
		})
		var pgErr *pgconn.PgError
		require.ErrorAs(t, err, &pgErr)
		require.Equal(t, "25006", pgErr.Code, "read_only_sql_transaction: %v", err)
	})

	t.Run("guards", func(t *testing.T) {
		require.ErrorIs(t, pool.WithUserTx(ctx, uuid.Nil, func(context.Context, pgx.Tx) error { return nil }), postgres.ErrNoUser)
		batch, err := postgres.Open(ctx, postgres.Config{URL: db.BatchURL, ApplicationName: "batch", MaxConns: 1})
		require.NoError(t, err)
		defer func() { require.NoError(t, batch.Shutdown(context.WithoutCancel(ctx))) }()
		require.ErrorIs(t, batch.WithUserTx(ctx, alice, func(context.Context, pgx.Tx) error { return nil }), postgres.ErrBypassRLS)
	})
}
