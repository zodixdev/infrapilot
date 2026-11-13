//go:build integration

package db

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/zodixdev/infrapilot/services/control-plane/internal/testdb"
)

// TestExplain_KeyQueries runs EXPLAIN (ANALYZE, BUFFERS) for every list endpoint's query and the per-request
// authorization lookups, as the runtime role with RLS active, over a data set shaped like the capacity design
// point (1,000 orgs). It fails on sequential scans and, when EXPLAIN_EVIDENCE_DIR is set, writes each plan there
// for the milestone report (AGENTS.md §7).
func TestExplain_KeyQueries(t *testing.T) {
	d := testdb.Start(t)
	ctx := t.Context()
	su, err := pgx.Connect(ctx, d.SuperuserURL)
	require.NoError(t, err)
	defer func() { _ = su.Close(context.WithoutCancel(ctx)) }()

	// 1,000 orgs × 5 projects × 20 resources (100k), 2 keys per project, 50 audit events per org, 2,000 users
	// with 3 memberships each. Seeded as superuser (bulk, bypassing RLS); ids are UUIDv7-like (time-ordered).
	seed := []string{
		`CREATE TEMP TABLE o AS SELECT g AS n, (lpad(to_hex(g), 8, '0') || '-0000-7000-8000-000000000000')::uuid AS id FROM generate_series(1, 1000) g`,
		`INSERT INTO organizations (id, name) SELECT id, 'org ' || n FROM o`,
		`INSERT INTO users (id, subject, email) SELECT (lpad(to_hex(g), 8, '0') || '-0000-7000-8000-000000000001')::uuid, 'sub-' || g, 'u' || g || '@example.com' FROM generate_series(1, 2000) g`,
		`INSERT INTO memberships (organization_id, user_id, role)
		   SELECT o.id, (lpad(to_hex(((o.n * 3 + k) % 2000) + 1), 8, '0') || '-0000-7000-8000-000000000001')::uuid,
		          (ARRAY['owner','admin','operator','viewer'])[1 + (k % 4)]
		   FROM o, generate_series(0, 5) k ON CONFLICT DO NOTHING`,
		`INSERT INTO projects (id, organization_id, name, slug)
		   SELECT gen_random_uuid(), o.id, 'project ' || p, 'project-' || p FROM o, generate_series(1, 5) p`,
		`INSERT INTO resources (id, organization_id, project_id, kind, name)
		   SELECT gen_random_uuid(), p.organization_id, p.id, (ARRAY['host','service','endpoint','database'])[1 + r % 4], 'res-' || r
		   FROM projects p, generate_series(1, 20) r`,
		`INSERT INTO api_keys (id, organization_id, project_id, name, prefix, scopes, created_by)
		   SELECT gen_random_uuid(), p.organization_id, p.id, 'key ' || k, 'ipk_' || substr(md5(p.id::text || k), 1, 8), ARRAY['read'], gen_random_uuid()
		   FROM projects p, generate_series(1, 2) k`,
		`INSERT INTO api_key_index (key_hash, key_id, organization_id, project_id, scopes)
		   SELECT sha256(k.id::text::bytea), k.id, k.organization_id, k.project_id, k.scopes FROM api_keys k`,
		`INSERT INTO audit_events (id, organization_id, actor_kind, actor_id, action, target_type, target_id)
		   SELECT gen_random_uuid(), o.id, 'user', gen_random_uuid(), 'member.added', 'member', e::text FROM o, generate_series(1, 50) e`,
		`ANALYZE`,
	}
	for _, s := range seed {
		_, err := su.Exec(ctx, s)
		require.NoError(t, err, s)
	}

	// Pick a mid-range org, one of its projects, a member and a key.
	var org, project, user, key uuid.UUID
	var keyHash []byte
	require.NoError(t, su.QueryRow(ctx, `SELECT id FROM organizations ORDER BY id OFFSET 500 LIMIT 1`).Scan(&org))
	require.NoError(t, su.QueryRow(ctx, `SELECT id FROM projects WHERE organization_id = $1 LIMIT 1`, org).Scan(&project))
	require.NoError(t, su.QueryRow(ctx, `SELECT user_id FROM memberships WHERE organization_id = $1 LIMIT 1`, org).Scan(&user))
	require.NoError(t, su.QueryRow(ctx, `SELECT key_id, key_hash FROM api_key_index WHERE organization_id = $1 LIMIT 1`, org).Scan(&key, &keyHash))

	maxUUID := uuid.Max
	type plan struct {
		name, sql string
		args      []any
		tx        string // "tenant", "user" or "system"
	}
	plans := []plan{
		{"ListOrganizationsForUser", listOrganizationsForUser, []any{user, uuid.Nil, int32(50)}, "user"},
		{"GetRole", getRole, []any{org, user}, "tenant"},
		{"ListMembers", listMembers, []any{org, uuid.Nil, int32(50)}, "tenant"},
		{"ListProjects", listProjects, []any{org, uuid.Nil, int32(50)}, "tenant"},
		{"ListResources", listResources, []any{org, project, uuid.Nil, int32(50)}, "tenant"},
		{"ListResourcesByKind", listResourcesByKind, []any{org, project, "host", uuid.Nil, int32(50)}, "tenant"},
		{"ListAPIKeys", listAPIKeys, []any{org, uuid.Nil, int32(50)}, "tenant"},
		{"ListProjectAPIKeys", listProjectAPIKeys, []any{org, project, uuid.Nil, int32(50)}, "tenant"},
		{"ListAuditEvents", listAuditEvents, []any{org, maxUUID, int32(50)}, "tenant"},
		{"LookupAPIKey", lookupAPIKey, []any{keyHash}, "system"},
		{"GetActiveAPIKeyScopes", getActiveAPIKeyScopes, []any{org, key}, "tenant"},
	}
	outDir := os.Getenv("EXPLAIN_EVIDENCE_DIR")
	for _, p := range plans {
		t.Run(p.name, func(t *testing.T) {
			var lines []string
			run := func(ctx context.Context, tx pgx.Tx) error {
				rows, err := tx.Query(ctx, "EXPLAIN (ANALYZE, BUFFERS) "+p.sql, p.args...)
				if err != nil {
					return err
				}
				lines, err = pgx.CollectRows(rows, pgx.RowTo[string])
				return err
			}
			switch p.tx {
			case "tenant":
				require.NoError(t, d.App.WithTx(ctx, org, run))
			case "user":
				require.NoError(t, d.App.WithUserTx(ctx, user, run))
			default:
				require.NoError(t, d.App.WithSystemTx(ctx, run))
			}
			text := strings.Join(lines, "\n")
			require.NotContains(t, text, "Seq Scan", "plan for %s:\n%s", p.name, text)
			require.Contains(t, text, "Index", "plan for %s:\n%s", p.name, text)
			if outDir != "" {
				require.NoError(t, os.MkdirAll(outDir, 0o750))
				doc := "-- " + p.name + " (" + p.tx + " transaction, role " + "svc_app" + ")\n" + strings.TrimSpace(p.sql) + "\n\n" + text + "\n"
				require.NoError(t, os.WriteFile(filepath.Join(outDir, p.name+".txt"), []byte(doc), 0o600))
			}
		})
	}
}
