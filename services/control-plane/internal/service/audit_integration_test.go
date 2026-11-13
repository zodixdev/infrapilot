//go:build integration

package service_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/zodixdev/infrapilot/services/control-plane/internal/domain"
	"github.com/zodixdev/infrapilot/services/control-plane/internal/service"
)

type auditRow struct {
	actorKind, action, targetType, targetID, requestID string
	actorID                                            uuid.UUID
	details                                            map[string]string
}

// auditRows reads the organization's audit trail straight from the table as the runtime role (RLS applies), so
// events of a deleted organization — no longer listable through the API — are still verifiable.
func (e *env) auditRows(t *testing.T, org uuid.UUID) []auditRow {
	t.Helper()
	var out []auditRow
	require.NoError(t, e.db.App.WithTx(t.Context(), org, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT actor_kind, actor_id, action, target_type, target_id, details, request_id
			FROM audit_events WHERE organization_id = $1 ORDER BY id`, org)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r auditRow
			var raw []byte
			if err := rows.Scan(&r.actorKind, &r.actorID, &r.action, &r.targetType, &r.targetID, &raw, &r.requestID); err != nil {
				return err
			}
			if err := json.Unmarshal(raw, &r.details); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	}))
	return out
}

// TestAudit_LifecycleEvents_Integration: every security-relevant change outside memberships (projects, API keys,
// the organization itself) leaves exactly one audit event with actor, target, details and the request id —
// including the organization deletion, which is recorded in the same transaction as the soft delete.
func TestAudit_LifecycleEvents_Integration(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	alice := e.user("alice")
	org := e.org(alice, "Acme")
	admin := e.member(alice, org, "admin", domain.RoleAdmin)

	var projectID, keyID uuid.UUID
	var keyPrefix string
	steps := []struct {
		name       string
		actor      service.Actor
		run        func(ctx context.Context) (target string)
		action     string
		targetType string
		details    func() map[string]string
	}{
		{"project created", admin, func(ctx context.Context) string {
			p, err := e.svc.CreateProject(ctx, admin, org, service.CreateProjectInput{Name: "Web", Slug: "web"})
			require.NoError(t, err)
			projectID = p.ID
			return p.ID.String()
		}, "project.created", "project", func() map[string]string { return map[string]string{"slug": "web"} }},
		{"api key created", admin, func(ctx context.Context) string {
			k, err := e.svc.CreateAPIKey(ctx, admin, org, service.CreateAPIKeyInput{ProjectID: projectID, Name: "ci", Scopes: []domain.Scope{domain.ScopeRead}})
			require.NoError(t, err)
			keyID, keyPrefix = k.Key.ID, k.Key.Prefix
			return k.Key.ID.String()
		}, "apikey.created", "api_key", nil},
		{"api key revoked", admin, func(ctx context.Context) string {
			_, err := e.svc.RevokeAPIKey(ctx, admin, org, keyID)
			require.NoError(t, err)
			return keyID.String()
		}, "apikey.revoked", "api_key", func() map[string]string { return map[string]string{"prefix": keyPrefix} }},
		{"project deleted", admin, func(ctx context.Context) string {
			_, _ = e.key(alice, org, projectID, domain.ScopeWrite) // one live key, revoked by the delete
			require.NoError(t, e.svc.DeleteProject(ctx, admin, org, projectID))
			return projectID.String()
		}, "project.deleted", "project", func() map[string]string { return map[string]string{"revoked_api_keys": "1"} }},
		{"organization deleted", alice, func(ctx context.Context) string {
			p := e.project(alice, org, "api")
			_, _ = e.key(alice, org, p, domain.ScopeRead)
			_, _ = e.key(alice, org, p, domain.ScopeWrite)
			require.NoError(t, e.svc.DeleteOrganization(ctx, alice, org))
			return org.String()
		}, "org.deleted", "organization", func() map[string]string { return map[string]string{"revoked_api_keys": "2"} }},
	}
	for _, s := range steps {
		t.Run(s.name, func(t *testing.T) {
			reqID := "req-" + s.action
			target := s.run(service.WithRequestMeta(ctx, service.RequestMeta{RequestID: reqID}))
			var matched []auditRow
			for _, r := range e.auditRows(t, org) {
				if r.action == s.action && r.targetID == target {
					matched = append(matched, r)
				}
			}
			require.Len(t, matched, 1, "exactly one %s event for %s", s.action, target)
			got := matched[0]
			require.Equal(t, string(domain.ActorUser), got.actorKind)
			require.Equal(t, s.actor.ID, got.actorID)
			require.Equal(t, s.targetType, got.targetType)
			require.Equal(t, reqID, got.requestID)
			if s.details != nil {
				require.Equal(t, s.details(), got.details)
			}
			for k := range got.details {
				require.NotContains(t, []string{"secret", "key", "hash", "key_sha256"}, k, "audit details never carry key material")
			}
		})
	}
}

// TestPagination_ForeignCursor_Integration: a page token minted while listing another organization grants
// nothing — the list stays confined to the caller's organization (RLS + explicit organization predicate).
func TestPagination_ForeignCursor_Integration(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	alice, bob := e.user("alice"), e.user("bob")
	orgA, orgB := e.org(alice, "A"), e.org(bob, "B")
	mine := map[uuid.UUID]bool{}
	for _, slug := range []string{"aaa1", "aaa2", "aaa3"} {
		mine[e.project(alice, orgA, slug)] = true
	}
	for _, slug := range []string{"bbb1", "bbb2", "bbb3", "bbb4"} {
		e.project(bob, orgB, slug)
	}
	_, foreignToken, err := e.svc.ListProjects(ctx, bob, orgB, 1, "")
	require.NoError(t, err)
	require.NotEmpty(t, foreignToken)

	token := foreignToken
	for pages := 0; ; pages++ {
		require.Less(t, pages, 10)
		page, next, listErr := e.svc.ListProjects(ctx, alice, orgA, 1, token)
		if listErr != nil {
			var ve *domain.ValidationError
			require.ErrorAs(t, listErr, &ve, "a foreign token is either rejected as invalid or confined to the caller's rows")
			return
		}
		for _, p := range page {
			require.True(t, mine[p.ID], "project %s of another organization leaked through a foreign cursor", p.ID)
		}
		if next == "" {
			break
		}
		token = next
	}
	_, _, err = e.svc.ListProjects(ctx, bob, orgA, 1, foreignToken)
	require.ErrorIs(t, err, domain.ErrNotFound, "a valid token never substitutes for membership")
}
