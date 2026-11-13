package pgtest

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

// AssertTenantSchema fails the test when TenantSchemaViolations reports anything. Every service runs it
// after applying its migrations (ADR-0018).
func AssertTenantSchema(t *testing.T, url, appRole string, nonTenant, allowedDefiners []string) {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), url)
	require.NoError(t, err)
	defer func() { _ = conn.Close(context.WithoutCancel(t.Context())) }()
	violations, err := TenantSchemaViolations(t.Context(), conn, appRole, nonTenant, allowedDefiners)
	require.NoError(t, err)
	require.Empty(t, violations, "schema violates ADR-0018")
}

// TenantSchemaViolations checks schema public against the ADR-0018 rules.
//
// A table is a tenant table when it has an organization_id column. For each tenant table:
//   - row-level security is enabled and at least one policy references app.org_id;
//   - appRole holds SELECT;
//   - every UNIQUE index (other than the primary key) and every foreign key includes organization_id, so
//     constraint errors cannot reveal another tenant's rows. Primary keys are exempt because they must be
//     server-generated surrogates (UUIDv7 or identity) that a client can never choose.
//
// For the whole schema: no view without security_invoker (views owned by the migrator bypass RLS) and no
// SECURITY DEFINER function unless listed in allowedDefiners. nonTenant lists tables that are not tenant data:
// tables without organization_id (outbox, processed_messages, global identities) and the pre-tenant lookup
// tables of ADR-0018, which carry organization_id as the answer of the lookup but are reached only through
// system transactions. Foreign keys from tenant tables to nonTenant tables are allowed: a violation reveals
// nothing about another tenant.
func TenantSchemaViolations(ctx context.Context, conn *pgx.Conn, appRole string, nonTenant, allowedDefiners []string) ([]string, error) {
	var v []string
	tables, err := strings(ctx, conn, `
		SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p') ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	tenant, err := strings(ctx, conn, `
		SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		JOIN pg_attribute a ON a.attrelid = c.oid AND a.attname = 'organization_id' AND NOT a.attisdropped
		WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p') ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	for _, tbl := range tables {
		if !slices.Contains(tenant, tbl) && !slices.Contains(nonTenant, tbl) {
			v = append(v, fmt.Sprintf("table %q has no organization_id and is not declared non-tenant", tbl))
		}
	}
	tenant = slices.DeleteFunc(tenant, func(tbl string) bool { return slices.Contains(nonTenant, tbl) })
	for _, tbl := range tenant {
		var rls, canSelect bool
		var policies int
		if scanErr := conn.QueryRow(ctx, `
			SELECT c.relrowsecurity,
			       has_table_privilege($2, c.oid, 'SELECT'),
			       (SELECT count(*) FROM pg_policies p WHERE p.schemaname = 'public' AND p.tablename = c.relname
			          AND (p.qual LIKE '%app.org_id%' OR p.with_check LIKE '%app.org_id%'))
			FROM pg_class c WHERE c.oid = to_regclass('public.' || quote_ident($1))`, tbl, appRole).Scan(&rls, &canSelect, &policies); scanErr != nil {
			return nil, fmt.Errorf("inspect %s: %w", tbl, scanErr)
		}
		if !rls {
			v = append(v, fmt.Sprintf("tenant table %q must ENABLE ROW LEVEL SECURITY", tbl))
		}
		if policies == 0 {
			v = append(v, fmt.Sprintf("tenant table %q needs a policy on app.org_id", tbl))
		}
		if !canSelect {
			v = append(v, fmt.Sprintf("role %s needs SELECT on tenant table %q (grant it in the migration)", appRole, tbl))
		}
		badUnique, err2 := strings(ctx, conn, `
			SELECT i.relname FROM pg_index x
			JOIN pg_class i ON i.oid = x.indexrelid
			JOIN pg_attribute a ON a.attrelid = x.indrelid AND a.attname = 'organization_id'
			WHERE x.indrelid = to_regclass('public.' || quote_ident($1)) AND x.indisunique AND NOT x.indisprimary
			  AND NOT (a.attnum = ANY (x.indkey))
			ORDER BY 1`, tbl)
		if err2 != nil {
			return nil, err2
		}
		for _, i := range badUnique {
			v = append(v, fmt.Sprintf("unique index %q on tenant table %q must include organization_id", i, tbl))
		}
		badFK, err3 := strings(ctx, conn, `
			SELECT con.conname FROM pg_constraint con
			JOIN pg_attribute a ON a.attrelid = con.conrelid AND a.attname = 'organization_id'
			JOIN pg_class ref ON ref.oid = con.confrelid
			WHERE con.conrelid = to_regclass('public.' || quote_ident($1)) AND con.contype = 'f' AND NOT (a.attnum = ANY (con.conkey))
			  AND NOT (ref.relname = ANY (COALESCE($2::text[], ARRAY[]::text[])))
			ORDER BY 1`, tbl, nonTenant)
		if err3 != nil {
			return nil, err3
		}
		for _, fk := range badFK {
			v = append(v, fmt.Sprintf("foreign key %q on tenant table %q must include organization_id", fk, tbl))
		}
	}
	views, err := strings(ctx, conn, `
		SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND c.relkind IN ('v', 'm')
		AND NOT coalesce('security_invoker=true' = ANY (c.reloptions), false) ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	for _, view := range views {
		v = append(v, fmt.Sprintf("view %q must be created WITH (security_invoker = true)", view))
	}
	definers, err := strings(ctx, conn, `
		SELECT p.proname FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE n.nspname = 'public' AND p.prosecdef ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	for _, d := range definers {
		if !slices.Contains(allowedDefiners, d) {
			v = append(v, fmt.Sprintf("SECURITY DEFINER function %q is not on the reviewed allow-list", d))
		}
	}
	return v, nil
}

func strings(ctx context.Context, conn *pgx.Conn, sql string, args ...any) ([]string, error) {
	rows, err := conn.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}
