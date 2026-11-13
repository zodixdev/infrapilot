//go:build integration

package store_test

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"

	"github.com/zodixdev/infrapilot/pkg/testutil/pgtest"
	"github.com/zodixdev/infrapilot/services/control-plane/internal/store/db"
	"github.com/zodixdev/infrapilot/services/control-plane/internal/testdb"
	"github.com/zodixdev/infrapilot/services/control-plane/migrations"
)

// tenantTables lists every tenant table; TestSchema verifies the list is complete.
var tenantTables = []string{"organizations", "memberships", "projects", "resources", "api_keys", "audit_events", "idempotency_keys"}

type tenant struct {
	org, user, project, resource, key uuid.UUID
	keyHash                           []byte
}

// seedTenant creates one organization with a member, project, resource, API key, audit event and idempotency
// record through the runtime role, exactly as the service does.
func seedTenant(t *testing.T, d *testdb.DB, name string) tenant {
	t.Helper()
	ctx := t.Context()
	tn := tenant{org: uuid.Must(uuid.NewV7()), project: uuid.Must(uuid.NewV7()), resource: uuid.Must(uuid.NewV7()), key: uuid.Must(uuid.NewV7())}
	h := sha256.Sum256([]byte("ipk_" + name))
	tn.keyHash = h[:]
	require.NoError(t, d.App.WithSystemTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		u, err := db.New(tx).UpsertUser(ctx, db.UpsertUserParams{ID: uuid.Must(uuid.NewV7()), Subject: "sub-" + name, Email: name + "@example.com", DisplayName: name})
		tn.user = u.ID
		return err
	}))
	require.NoError(t, d.App.WithTx(ctx, tn.org, func(ctx context.Context, tx pgx.Tx) error {
		q := db.New(tx)
		if _, err := q.InsertOrganization(ctx, db.InsertOrganizationParams{ID: tn.org, Name: name}); err != nil {
			return err
		}
		if err := q.InsertMembership(ctx, db.InsertMembershipParams{OrganizationID: tn.org, UserID: tn.user, Role: "owner"}); err != nil {
			return err
		}
		if _, err := q.InsertProject(ctx, db.InsertProjectParams{ID: tn.project, OrganizationID: tn.org, Name: "web", Slug: "web"}); err != nil {
			return err
		}
		if _, err := q.InsertResource(ctx, db.InsertResourceParams{ID: tn.resource, OrganizationID: tn.org, ProjectID: tn.project, Kind: "host", Name: "db-1", Labels: []byte(`{"env":"prod"}`)}); err != nil {
			return err
		}
		if _, err := q.InsertAPIKey(ctx, db.InsertAPIKeyParams{ID: tn.key, OrganizationID: tn.org, ProjectID: tn.project, Name: "ci", Prefix: "ipk_abcdefgh", Scopes: []string{"read"}, CreatedBy: tn.user}); err != nil {
			return err
		}
		if err := q.InsertAPIKeyIndex(ctx, db.InsertAPIKeyIndexParams{KeyHash: tn.keyHash, KeyID: tn.key, OrganizationID: tn.org, ProjectID: tn.project, Scopes: []string{"read"}}); err != nil {
			return err
		}
		if err := q.InsertAuditEvent(ctx, db.InsertAuditEventParams{ID: uuid.Must(uuid.NewV7()), OrganizationID: tn.org, ActorKind: "user", ActorID: tn.user, Action: "member.added", TargetType: "member", TargetID: tn.user.String(), Details: []byte(`{}`)}); err != nil {
			return err
		}
		_, err := q.InsertIdempotencyKey(ctx, db.InsertIdempotencyKeyParams{OrganizationID: tn.org, PrincipalID: tn.user, Key: "k1", Operation: "CreateProject", RequestHash: h[:], ResourceID: tn.project})
		return err
	}))
	return tn
}

func count(ctx context.Context, tx pgx.Tx, table, where string, args ...any) (int64, error) {
	var n int64
	err := tx.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %s %s`, pgx.Identifier{table}.Sanitize(), where), args...).Scan(&n)
	return n, err
}

func TestSchema_Integration(t *testing.T) {
	d := testdb.Start(t)
	pgtest.AssertTenantSchema(t, d.MigratorURL, pgtest.AppRole, testdb.NonTenantTables, nil)

	// Re-running is a no-op (Helm hooks run on every upgrade).
	require.NoError(t, migrations.Migrate(t.Context(), d.MigratorURL, slog.New(slog.DiscardHandler)))

	conn, err := pgx.Connect(t.Context(), d.MigratorURL)
	require.NoError(t, err)
	defer func() { _ = conn.Close(context.WithoutCancel(t.Context())) }()
	rows, err := conn.Query(t.Context(), `SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		JOIN pg_attribute a ON a.attrelid = c.oid AND a.attname = 'organization_id'
		WHERE n.nspname = 'public' AND c.relkind = 'r' AND c.relname <> ALL ($1) ORDER BY 1`, testdb.NonTenantTables)
	require.NoError(t, err)
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)
	require.ElementsMatch(t, tenantTables, got, "tenantTables must list every tenant table so the cross-tenant suite covers it")
}

func TestMigrate_RefusesNonMigratorRole_Integration(t *testing.T) {
	pg := pgtest.Start(t)
	err := migrations.Migrate(t.Context(), pg.AppURL, slog.New(slog.DiscardHandler))
	require.Error(t, err, "the runtime role can neither own tables nor run migrations")
}

func TestRLS_CrossTenant_Integration(t *testing.T) {
	d := testdb.Start(t)
	a, b := seedTenant(t, d, "alpha"), seedTenant(t, d, "beta")
	ctx := t.Context()

	for _, table := range tenantTables {
		t.Run(table, func(t *testing.T) {
			require.NoError(t, d.App.WithTx(ctx, a.org, func(ctx context.Context, tx pgx.Tx) error {
				own, err := count(ctx, tx, table, "")
				require.NoError(t, err)
				require.Positive(t, own, "seed must create a row in %s", table)
				foreign, err := count(ctx, tx, table, "WHERE organization_id = $1", b.org)
				require.NoError(t, err)
				require.Zero(t, foreign, "org A must not read org B's %s", table)
				return nil
			}))

			// Updates of another tenant's rows match nothing (on a column the runtime role may update).
			require.NoError(t, d.App.WithTx(ctx, a.org, func(ctx context.Context, tx pgx.Tx) error {
				stmt, ok := map[string]string{
					"organizations": `UPDATE organizations SET name = name WHERE id = $1`,
					"memberships":   `UPDATE memberships SET role = role WHERE organization_id = $1`,
					"projects":      `UPDATE projects SET name = name WHERE organization_id = $1`,
					"resources":     `UPDATE resources SET name = name WHERE organization_id = $1`,
					"api_keys":      `UPDATE api_keys SET revoked_at = revoked_at WHERE organization_id = $1`,
				}[table]
				if !ok {
					return nil // append-only tables have no UPDATE grant; covered by TestAudit_AppendOnly
				}
				tag, err := tx.Exec(ctx, stmt, b.org)
				require.NoError(t, err)
				require.Zero(t, tag.RowsAffected())
				return nil
			}))

			// The unset tenant sees nothing (fails closed).
			require.NoError(t, d.App.WithSystemTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
				n, err := count(ctx, tx, table, "")
				require.NoError(t, err)
				require.Zero(t, n, "system transactions must not see %s rows", table)
				return nil
			}))
		})
	}

	t.Run("writing a row into another tenant violates the policy", func(t *testing.T) {
		err := d.App.WithTx(ctx, a.org, func(ctx context.Context, tx pgx.Tx) error {
			_, err := db.New(tx).InsertProject(ctx, db.InsertProjectParams{ID: uuid.Must(uuid.NewV7()), OrganizationID: b.org, Name: "x", Slug: "smuggled"})
			return err
		})
		requireSQLState(t, err, "42501")
	})

	t.Run("membership deletes cannot cross tenants", func(t *testing.T) {
		require.NoError(t, d.App.WithTx(ctx, a.org, func(ctx context.Context, tx pgx.Tx) error {
			n, err := db.New(tx).DeleteMember(ctx, db.DeleteMemberParams{OrganizationID: b.org, UserID: b.user})
			require.Zero(t, n)
			return err
		}))
	})

	t.Run("lookup table answers only the key's tenant ids", func(t *testing.T) {
		require.NoError(t, d.App.WithSystemTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			row, err := db.New(tx).LookupAPIKey(ctx, b.keyHash)
			require.NoError(t, err)
			require.Equal(t, b.org, row.OrganizationID)
			require.Equal(t, b.project, row.ProjectID)
			return nil
		}))
	})

	t.Run("user context lists only the user's organizations", func(t *testing.T) {
		require.NoError(t, d.App.WithUserTx(ctx, a.user, func(ctx context.Context, tx pgx.Tx) error {
			orgs, err := db.New(tx).ListOrganizationsForUser(ctx, db.ListOrganizationsForUserParams{UserID: a.user, After: uuid.Nil, Lim: 50})
			require.NoError(t, err)
			require.Len(t, orgs, 1)
			require.Equal(t, a.org, orgs[0].ID)
			require.Equal(t, "owner", orgs[0].Role)
			// Even naming the other user explicitly returns nothing: RLS binds rows to app.user_id.
			others, err := db.New(tx).ListOrganizationsForUser(ctx, db.ListOrganizationsForUserParams{UserID: b.user, After: uuid.Nil, Lim: 50})
			require.NoError(t, err)
			require.Empty(t, others)
			// Projects have no user policy: nothing is visible in a user transaction.
			n, err := count(ctx, tx, "projects", "")
			require.NoError(t, err)
			require.Zero(t, n)
			return nil
		}))
	})
}

func TestAPIKeyRevocation_MirroredToLookup_Integration(t *testing.T) {
	d := testdb.Start(t)
	a := seedTenant(t, d, "alpha")
	ctx := t.Context()
	require.NoError(t, d.App.WithTx(ctx, a.org, func(ctx context.Context, tx pgx.Tx) error {
		_, err := db.New(tx).RevokeAPIKey(ctx, db.RevokeAPIKeyParams{OrganizationID: a.org, ID: a.key})
		return err
	}))
	require.NoError(t, d.App.WithSystemTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := db.New(tx).LookupAPIKey(ctx, a.keyHash)
		require.ErrorIs(t, err, pgx.ErrNoRows, "a revoked key no longer authenticates")
		return nil
	}))
}

func TestColumnGrants_Integration(t *testing.T) {
	d := testdb.Start(t)
	a := seedTenant(t, d, "alpha")
	for _, stmt := range []string{
		`UPDATE api_keys SET scopes = ARRAY['write']`,
		`UPDATE api_key_index SET organization_id = gen_random_uuid()`,
		`UPDATE users SET subject = 'stolen'`,
		`UPDATE memberships SET user_id = gen_random_uuid()`,
	} {
		err := d.App.WithTx(t.Context(), a.org, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, stmt)
			return err
		})
		requireSQLState(t, err, "42501")
	}
}

func TestAudit_AppendOnly_Integration(t *testing.T) {
	d := testdb.Start(t)
	a := seedTenant(t, d, "alpha")
	for _, stmt := range []string{
		`UPDATE audit_events SET action = 'member.removed'`,
		`DELETE FROM audit_events`,
		`UPDATE idempotency_keys SET key = 'x'`,
	} {
		err := d.App.WithTx(t.Context(), a.org, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, stmt)
			return err
		})
		requireSQLState(t, err, "42501") // insufficient_privilege: no grant
	}
}

func requireSQLState(t *testing.T, err error, code string) {
	t.Helper()
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	require.Equal(t, code, pgErr.Code, "%v", err)
}
