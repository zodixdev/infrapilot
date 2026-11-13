//go:build integration

package service_test

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/zodixdev/infrapilot/services/control-plane/internal/domain"
	"github.com/zodixdev/infrapilot/services/control-plane/internal/service"
)

// fixture is one organization with a member of every role, an outsider and API keys of every scope.
type fixture struct {
	org, project, resource, apiKey uuid.UUID
	actors                         map[string]service.Actor
}

var matrixActors = []string{"owner", "admin", "operator", "viewer", "outsider", "key-read", "key-write", "key-ingest"}

func (e *env) fixture(t *testing.T, n int) fixture {
	t.Helper()
	ctx := t.Context()
	owner := e.user("owner")
	f := fixture{actors: map[string]service.Actor{"owner": owner}}
	f.org = e.org(owner, fmt.Sprintf("org-%d", n))
	for _, r := range []domain.Role{domain.RoleAdmin, domain.RoleOperator, domain.RoleViewer} {
		f.actors[string(r)] = e.member(owner, f.org, string(r), r)
	}
	f.actors["outsider"] = e.user("outsider")
	f.project = e.project(owner, f.org, "web")
	f.actors["key-read"], _ = e.key(owner, f.org, f.project, domain.ScopeRead)
	f.actors["key-write"], _ = e.key(owner, f.org, f.project, domain.ScopeWrite)
	f.actors["key-ingest"], _ = e.key(owner, f.org, f.project, domain.ScopeIngest)
	r, err := e.svc.CreateResource(ctx, owner, f.org, f.project, service.CreateResourceInput{Kind: domain.KindHost, Name: "db-1"})
	require.NoError(t, err)
	f.resource = r.ID
	k, err := e.svc.CreateAPIKey(ctx, owner, f.org, service.CreateAPIKeyInput{ProjectID: f.project, Name: "target", Scopes: []domain.Scope{domain.ScopeRead}})
	require.NoError(t, err)
	f.apiKey = k.Key.ID
	return f
}

// TestRBAC_Matrix exercises every operation with every kind of caller against the ADR-0022 matrix. Allowed
// callers succeed; members and keys of the organization lacking the permission get PERMISSION_DENIED; outsiders
// get NOT_FOUND (an organization id reveals nothing).
func TestRBAC_Matrix_Integration(t *testing.T) {
	e := newEnv(t)
	all := []string{"owner", "admin", "operator", "viewer"}
	managers := []string{"owner", "admin"}
	readers := append(slices.Clone(all), "key-read", "key-write")
	ops := []struct {
		name    string
		allowed []string
		run     func(ctx context.Context, f fixture, a service.Actor) error
	}{
		{"GetOrganization", readers, func(ctx context.Context, f fixture, a service.Actor) error {
			_, err := e.svc.GetOrganization(ctx, a, f.org)
			return err
		}},
		{"UpdateOrganization", managers, func(ctx context.Context, f fixture, a service.Actor) error {
			n := "renamed"
			_, err := e.svc.UpdateOrganization(ctx, a, f.org, service.UpdateOrganizationInput{Name: &n})
			return err
		}},
		{"DeleteOrganization", []string{"owner"}, func(ctx context.Context, f fixture, a service.Actor) error {
			return e.svc.DeleteOrganization(ctx, a, f.org)
		}},
		{"ListMembers", all, func(ctx context.Context, f fixture, a service.Actor) error {
			_, _, err := e.svc.ListMembers(ctx, a, f.org, 0, "")
			return err
		}},
		{"GetMember", all, func(ctx context.Context, f fixture, a service.Actor) error {
			_, err := e.svc.GetMember(ctx, a, f.org, f.actors["viewer"].ID)
			return err
		}},
		{"AddMember", managers, func(ctx context.Context, f fixture, a service.Actor) error {
			e.user("newcomer")
			_, err := e.svc.AddMember(ctx, a, f.org, "newcomer@example.com", domain.RoleViewer)
			return err
		}},
		{"UpdateMemberRole(viewer→operator)", managers, func(ctx context.Context, f fixture, a service.Actor) error {
			_, err := e.svc.UpdateMemberRole(ctx, a, f.org, f.actors["viewer"].ID, domain.RoleOperator)
			return err
		}},
		{"RemoveMember(operator)", append(slices.Clone(managers), "operator"), func(ctx context.Context, f fixture, a service.Actor) error {
			// The operator removing itself is allowed (members may leave).
			return e.svc.RemoveMember(ctx, a, f.org, f.actors["operator"].ID)
		}},
		{"ListProjects", readers, func(ctx context.Context, f fixture, a service.Actor) error {
			_, _, err := e.svc.ListProjects(ctx, a, f.org, 0, "")
			return err
		}},
		{"GetProject", readers, func(ctx context.Context, f fixture, a service.Actor) error {
			_, err := e.svc.GetProject(ctx, a, f.org, f.project)
			return err
		}},
		{"CreateProject", managers, func(ctx context.Context, f fixture, a service.Actor) error {
			_, err := e.svc.CreateProject(ctx, a, f.org, service.CreateProjectInput{Name: "New", Slug: "new"})
			return err
		}},
		{"UpdateProject", managers, func(ctx context.Context, f fixture, a service.Actor) error {
			d := "updated"
			_, err := e.svc.UpdateProject(ctx, a, f.org, f.project, service.UpdateProjectInput{Description: &d})
			return err
		}},
		{"DeleteProject", managers, func(ctx context.Context, f fixture, a service.Actor) error {
			return e.svc.DeleteProject(ctx, a, f.org, f.project)
		}},
		{"ListResources", readers, func(ctx context.Context, f fixture, a service.Actor) error {
			_, _, err := e.svc.ListResources(ctx, a, f.org, f.project, "", 0, "")
			return err
		}},
		{"GetResource", readers, func(ctx context.Context, f fixture, a service.Actor) error {
			_, err := e.svc.GetResource(ctx, a, f.org, f.project, f.resource)
			return err
		}},
		{"CreateResource", []string{"owner", "admin", "operator", "key-write"}, func(ctx context.Context, f fixture, a service.Actor) error {
			_, err := e.svc.CreateResource(ctx, a, f.org, f.project, service.CreateResourceInput{Kind: domain.KindService, Name: "new"})
			return err
		}},
		{"UpdateResource", []string{"owner", "admin", "operator", "key-write"}, func(ctx context.Context, f fixture, a service.Actor) error {
			n := "renamed"
			_, err := e.svc.UpdateResource(ctx, a, f.org, f.project, f.resource, service.UpdateResourceInput{Name: &n})
			return err
		}},
		{"DeleteResource", []string{"owner", "admin", "operator", "key-write"}, func(ctx context.Context, f fixture, a service.Actor) error {
			return e.svc.DeleteResource(ctx, a, f.org, f.project, f.resource)
		}},
		{"ListAPIKeys", managers, func(ctx context.Context, f fixture, a service.Actor) error {
			_, _, err := e.svc.ListAPIKeys(ctx, a, f.org, uuid.Nil, 0, "")
			return err
		}},
		{"GetAPIKey", managers, func(ctx context.Context, f fixture, a service.Actor) error {
			_, err := e.svc.GetAPIKey(ctx, a, f.org, f.apiKey)
			return err
		}},
		{"CreateAPIKey", managers, func(ctx context.Context, f fixture, a service.Actor) error {
			_, err := e.svc.CreateAPIKey(ctx, a, f.org, service.CreateAPIKeyInput{ProjectID: f.project, Name: "n", Scopes: []domain.Scope{domain.ScopeRead}})
			return err
		}},
		{"RevokeAPIKey", managers, func(ctx context.Context, f fixture, a service.Actor) error {
			_, err := e.svc.RevokeAPIKey(ctx, a, f.org, f.apiKey)
			return err
		}},
		{"ListAuditEvents", managers, func(ctx context.Context, f fixture, a service.Actor) error {
			_, _, err := e.svc.ListAuditEvents(ctx, a, f.org, 0, "")
			return err
		}},
	}
	n := 0
	for _, op := range ops {
		t.Run(op.name, func(t *testing.T) {
			for _, who := range matrixActors {
				n++
				f := e.fixture(t, n)
				err := op.run(t.Context(), f, f.actors[who])
				switch {
				case slices.Contains(op.allowed, who):
					require.NoError(t, err, "%s must be allowed to %s", who, op.name)
				case who == "outsider":
					require.ErrorIs(t, err, domain.ErrNotFound, "outsiders learn nothing (%s)", op.name)
				default:
					require.ErrorIs(t, err, domain.ErrPermissionDenied, "%s must be denied %s", who, op.name)
				}
			}
		})
	}

	t.Run("CheckPermission agrees with the matrix", func(t *testing.T) {
		f := e.fixture(t, 9999)
		for _, who := range matrixActors {
			a := f.actors[who]
			for _, perm := range domain.Permissions {
				project := uuid.Nil
				if domain.ProjectScoped(perm) {
					project = f.project
				}
				d, err := e.svc.CheckPermission(t.Context(), a, f.org, perm, project)
				require.NoError(t, err)
				want := false
				switch {
				case who == "outsider":
				case len(who) > 4 && who[:4] == "key-":
					want = domain.KeyAllows([]domain.Scope{domain.Scope(who[4:])}, f.project.String(), project.String(), perm)
					if project == uuid.Nil {
						want = domain.KeyAllows([]domain.Scope{domain.Scope(who[4:])}, f.project.String(), "", perm)
					}
				default:
					want = domain.RoleAllows(domain.Role(who), perm)
					require.Equal(t, domain.Role(who), d.Role)
				}
				require.Equal(t, want, d.Allowed, "%s × %s", who, perm)
			}
		}
		d, err := e.svc.CheckPermission(t.Context(), f.actors["owner"], uuid.Must(uuid.NewV7()), domain.PermOrgRead, uuid.Nil)
		require.NoError(t, err)
		require.False(t, d.Allowed, "unknown organizations are simply not allowed")
	})
}
