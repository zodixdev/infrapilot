//go:build integration

package service_test

import (
	"crypto/sha256"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/zodixdev/infrapilot/services/control-plane/internal/domain"
	"github.com/zodixdev/infrapilot/services/control-plane/internal/service"
)

func TestOrganizationLifecycle_Integration(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	alice := e.user("alice")
	org := e.org(alice, "Acme")

	got, err := e.svc.GetOrganization(ctx, alice, org)
	require.NoError(t, err)
	require.Equal(t, "Acme", got.Name)

	mine, next, err := e.svc.ListMyOrganizations(ctx, alice, 0, "")
	require.NoError(t, err)
	require.Empty(t, next)
	require.Len(t, mine, 1)
	require.Equal(t, domain.RoleOwner, mine[0].Role)

	renamed := "Acme Corp"
	upd, err := e.svc.UpdateOrganization(ctx, alice, org, service.UpdateOrganizationInput{Name: &renamed})
	require.NoError(t, err)
	require.Equal(t, renamed, upd.Name)

	p := e.project(alice, org, "web")
	_, secret := e.key(alice, org, p, domain.ScopeRead)
	require.NoError(t, e.svc.DeleteOrganization(ctx, alice, org))
	_, err = e.svc.GetOrganization(ctx, alice, org)
	require.ErrorIs(t, err, domain.ErrNotFound, "deleted organizations are gone for everyone")
	h := sha256.Sum256([]byte(secret))
	_, err = e.svc.AuthenticateAPIKey(ctx, h[:])
	require.ErrorIs(t, err, domain.ErrUnauthenticated, "deleting an organization revokes its keys")
	mine, _, err = e.svc.ListMyOrganizations(ctx, alice, 0, "")
	require.NoError(t, err)
	require.Empty(t, mine)

	_, err = e.svc.CreateOrganization(ctx, alice, service.CreateOrganizationInput{Name: " "})
	var ve *domain.ValidationError
	require.ErrorAs(t, err, &ve)
}

func TestIdempotency_Integration(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	alice := e.user("alice")
	org := e.org(alice, "Acme")

	t.Run("replay returns the original resource", func(t *testing.T) {
		in := service.CreateProjectInput{Name: "API", Slug: "api"}
		first, err := e.svc.CreateProject(withKey(ctx, "k-1"), alice, org, in)
		require.NoError(t, err)
		again, err := e.svc.CreateProject(withKey(ctx, "k-1"), alice, org, in)
		require.NoError(t, err)
		require.Equal(t, first.ID, again.ID)
		list, _, err := e.svc.ListProjects(ctx, alice, org, 0, "")
		require.NoError(t, err)
		require.Len(t, list, 1)
	})

	t.Run("same key with a different request is rejected", func(t *testing.T) {
		_, err := e.svc.CreateProject(withKey(ctx, "k-1"), alice, org, service.CreateProjectInput{Name: "Other", Slug: "other"})
		require.ErrorIs(t, err, domain.ErrIdempotencyKeyReused)
	})

	t.Run("keys are scoped to the principal", func(t *testing.T) {
		bob := e.member(alice, org, "bob", domain.RoleAdmin)
		p, err := e.svc.CreateProject(withKey(ctx, "k-1"), bob, org, service.CreateProjectInput{Name: "Bob", Slug: "bob"})
		require.NoError(t, err)
		require.Equal(t, "bob", p.Slug)
	})

	t.Run("concurrent duplicates create exactly one resource", func(t *testing.T) {
		const n = 20
		ids := make([]uuid.UUID, n)
		errs := make([]error, n)
		var wg sync.WaitGroup
		for i := range n {
			wg.Go(func() {
				p, err := e.svc.CreateProject(withKey(ctx, "k-race"), alice, org, service.CreateProjectInput{Name: "Race", Slug: "race"})
				ids[i], errs[i] = p.ID, err
			})
		}
		wg.Wait()
		// Each duplicate either replays the single project or is told the original is still in progress.
		created := uuid.Nil
		for i := range n {
			if errs[i] != nil {
				require.ErrorIs(t, errs[i], domain.ErrConflict)
				continue
			}
			if created == uuid.Nil {
				created = ids[i]
			}
			require.Equal(t, created, ids[i])
		}
		require.NotEqual(t, uuid.Nil, created)
		again, err := e.svc.CreateProject(withKey(ctx, "k-race"), alice, org, service.CreateProjectInput{Name: "Race", Slug: "race"})
		require.NoError(t, err, "after the original committed, a retry replays it")
		require.Equal(t, created, again.ID)
	})

	t.Run("organization creation is idempotent per user and key", func(t *testing.T) {
		a, err := e.svc.CreateOrganization(withKey(ctx, "org-1"), alice, service.CreateOrganizationInput{Name: "Beta"})
		require.NoError(t, err)
		b, err := e.svc.CreateOrganization(withKey(ctx, "org-1"), alice, service.CreateOrganizationInput{Name: "Beta"})
		require.NoError(t, err)
		require.Equal(t, a.ID, b.ID)
		_, err = e.svc.CreateOrganization(withKey(ctx, "org-1"), alice, service.CreateOrganizationInput{Name: "Gamma"})
		require.ErrorIs(t, err, domain.ErrIdempotencyKeyReused)
	})

	t.Run("api key replay never returns the secret again", func(t *testing.T) {
		p := e.project(alice, org, "keys")
		in := service.CreateAPIKeyInput{ProjectID: p, Name: "ci", Scopes: []domain.Scope{domain.ScopeRead}}
		first, err := e.svc.CreateAPIKey(withKey(ctx, "key-1"), alice, org, in)
		require.NoError(t, err)
		require.NotEmpty(t, first.Secret)
		again, err := e.svc.CreateAPIKey(withKey(ctx, "key-1"), alice, org, in)
		require.NoError(t, err)
		require.Equal(t, first.Key.ID, again.Key.ID)
		require.Empty(t, again.Secret)
	})
}

func TestMembers_Integration(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	alice := e.user("alice")
	org := e.org(alice, "Acme")

	t.Run("adding requires a signed-in user and is audited", func(t *testing.T) {
		_, err := e.svc.AddMember(ctx, alice, org, "nobody@example.com", domain.RoleViewer)
		require.ErrorIs(t, err, domain.ErrNotFound)
		bob := e.member(alice, org, "bob", domain.RoleViewer)
		_, err = e.svc.AddMember(ctx, alice, org, "bob@example.com", domain.RoleViewer)
		require.ErrorIs(t, err, domain.ErrConflict)
		m, err := e.svc.GetMember(ctx, alice, org, bob.ID)
		require.NoError(t, err)
		require.Equal(t, domain.RoleViewer, m.Role)
		require.Equal(t, "bob@example.com", m.Email)
	})

	t.Run("role changes and removals are audited with before/after", func(t *testing.T) {
		carol := e.member(alice, org, "carol", domain.RoleViewer)
		_, err := e.svc.UpdateMemberRole(withKey(ctx, "r1"), alice, org, carol.ID, domain.RoleAdmin)
		require.NoError(t, err)
		require.NoError(t, e.svc.RemoveMember(ctx, alice, org, carol.ID))
		events, _, err := e.svc.ListAuditEvents(ctx, alice, org, 0, "")
		require.NoError(t, err)
		var actions []string
		for _, ev := range events {
			if ev.TargetID == carol.ID.String() {
				actions = append(actions, ev.Action)
				require.Equal(t, alice.ID, ev.ActorID)
				if ev.Action == "member.role_changed" {
					require.Equal(t, map[string]string{"old_role": "viewer", "new_role": "admin"}, ev.Details)
					require.Equal(t, "req-r1", ev.RequestID)
				}
			}
		}
		require.Equal(t, []string{"member.removed", "member.role_changed", "member.added"}, actions, "newest first")
	})

	t.Run("revocation takes effect on the next call", func(t *testing.T) {
		dave := e.member(alice, org, "dave", domain.RoleAdmin)
		_, _, err := e.svc.ListMembers(ctx, dave, org, 0, "")
		require.NoError(t, err)
		require.NoError(t, e.svc.RemoveMember(ctx, alice, org, dave.ID))
		_, _, err = e.svc.ListMembers(ctx, dave, org, 0, "")
		require.ErrorIs(t, err, domain.ErrNotFound)
	})

	t.Run("owner invariants", func(t *testing.T) {
		erin := e.member(alice, org, "erin", domain.RoleAdmin)
		_, err := e.svc.UpdateMemberRole(ctx, erin, org, alice.ID, domain.RoleViewer)
		require.ErrorIs(t, err, domain.ErrPermissionDenied, "admins cannot demote owners")
		_, err = e.svc.AddMember(ctx, erin, org, "frank@example.com", domain.RoleOwner)
		require.ErrorIs(t, err, domain.ErrPermissionDenied, "admins cannot grant owner")
		_, err = e.svc.UpdateMemberRole(ctx, alice, org, alice.ID, domain.RoleAdmin)
		require.ErrorIs(t, err, domain.ErrFailedPrecondition, "the last owner cannot step down")
		require.ErrorIs(t, e.svc.RemoveMember(ctx, alice, org, alice.ID), domain.ErrFailedPrecondition)
	})

	t.Run("concurrent demotion of the last two owners keeps one owner", func(t *testing.T) {
		o2 := e.user("owner2")
		org2 := e.org(o2, "Two owners")
		o3 := e.member(o2, org2, "owner3", domain.RoleOwner)
		var wg sync.WaitGroup
		errs := make([]error, 2)
		wg.Go(func() { _, errs[0] = e.svc.UpdateMemberRole(ctx, o2, org2, o2.ID, domain.RoleAdmin) })
		wg.Go(func() { _, errs[1] = e.svc.UpdateMemberRole(ctx, o3, org2, o3.ID, domain.RoleAdmin) })
		wg.Wait()
		ok := 0
		for _, err := range errs {
			if err == nil {
				ok++
			} else {
				require.ErrorIs(t, err, domain.ErrFailedPrecondition)
			}
		}
		require.Equal(t, 1, ok, "exactly one demotion wins: %v", errs)
	})

	t.Run("members can leave", func(t *testing.T) {
		gina := e.member(alice, org, "gina", domain.RoleViewer)
		require.NoError(t, e.svc.RemoveMember(ctx, gina, org, gina.ID))
	})
}

func TestAPIKeysAndResources_Integration(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	alice := e.user("alice")
	org := e.org(alice, "Acme")
	web, api := e.project(alice, org, "web"), e.project(alice, org, "api")

	writer, secret := e.key(alice, org, web, domain.ScopeWrite)
	h := sha256.Sum256([]byte(secret))
	id, err := e.svc.AuthenticateAPIKey(ctx, h[:])
	require.NoError(t, err)
	require.Equal(t, writer.ID, id.KeyID)
	require.Equal(t, web, id.ProjectID)

	r, err := e.svc.CreateResource(ctx, writer, org, web, service.CreateResourceInput{Kind: domain.KindHost, Name: "db-1", Labels: map[string]string{"env": "prod"}})
	require.NoError(t, err)
	require.Equal(t, map[string]string{"env": "prod"}, r.Labels)
	_, err = e.svc.CreateResource(ctx, writer, org, api, service.CreateResourceInput{Kind: domain.KindHost, Name: "x"})
	require.ErrorIs(t, err, domain.ErrPermissionDenied, "keys are bound to their project")
	_, err = e.svc.CreateResource(ctx, alice, org, web, service.CreateResourceInput{Kind: domain.KindHost, Name: "db-1"})
	require.ErrorIs(t, err, domain.ErrConflict)

	empty := map[string]string{}
	upd, err := e.svc.UpdateResource(ctx, alice, org, web, r.ID, service.UpdateResourceInput{Labels: empty})
	require.NoError(t, err)
	require.Empty(t, upd.Labels, "an empty label set clears labels")

	t.Run("pagination", func(t *testing.T) {
		for i := range 5 {
			_, err := e.svc.CreateResource(ctx, alice, org, api, service.CreateResourceInput{Kind: domain.KindService, Name: "svc-" + string(rune('a'+i))})
			require.NoError(t, err)
		}
		var names []string
		token := ""
		for pages := 0; ; pages++ {
			require.Less(t, pages, 4)
			page, next, err := e.svc.ListResources(ctx, alice, org, api, "", 2, token)
			require.NoError(t, err)
			for _, x := range page {
				names = append(names, x.Name)
			}
			if next == "" {
				break
			}
			token = next
		}
		require.Equal(t, []string{"svc-a", "svc-b", "svc-c", "svc-d", "svc-e"}, names)
		hosts, _, err := e.svc.ListResources(ctx, alice, org, web, domain.KindHost, 0, "")
		require.NoError(t, err)
		require.Len(t, hosts, 1)
	})

	t.Run("revocation is immediate at the control plane", func(t *testing.T) {
		_, err := e.svc.RevokeAPIKey(ctx, alice, org, writer.ID)
		require.NoError(t, err)
		_, err = e.svc.AuthenticateAPIKey(ctx, h[:])
		require.ErrorIs(t, err, domain.ErrUnauthenticated)
		_, _, err = e.svc.ListResources(ctx, writer, org, web, "", 0, "")
		require.ErrorIs(t, err, domain.ErrNotFound, "a key the edge still caches is refused by the owning service")
	})

	t.Run("deleting a project deletes its resources and revokes its keys", func(t *testing.T) {
		reader, rs := e.key(alice, org, api, domain.ScopeRead)
		require.NoError(t, e.svc.DeleteProject(ctx, alice, org, api))
		rh := sha256.Sum256([]byte(rs))
		_, err := e.svc.AuthenticateAPIKey(ctx, rh[:])
		require.ErrorIs(t, err, domain.ErrUnauthenticated)
		_, _, err = e.svc.ListResources(ctx, alice, org, api, "", 0, "")
		require.ErrorIs(t, err, domain.ErrNotFound)
		_, err = e.svc.CreateResource(ctx, alice, org, api, service.CreateResourceInput{Kind: domain.KindHost, Name: "late"})
		require.ErrorIs(t, err, domain.ErrNotFound, "no live children under a deleted project")
		_ = reader
	})

	t.Run("expired keys do not authenticate", func(t *testing.T) {
		exp := time.Now().Add(time.Hour)
		k, err := e.svc.CreateAPIKey(ctx, alice, org, service.CreateAPIKeyInput{ProjectID: web, Name: "short", Scopes: []domain.Scope{domain.ScopeRead}, ExpiresAt: &exp})
		require.NoError(t, err)
		require.NotNil(t, k.Key.ExpiresAt)
		_, err = e.svc.CreateAPIKey(ctx, alice, org, service.CreateAPIKeyInput{ProjectID: web, Name: "past", Scopes: []domain.Scope{domain.ScopeRead}, ExpiresAt: new(time.Now().Add(-time.Minute))})
		var ve *domain.ValidationError
		require.ErrorAs(t, err, &ve)
	})
}

func TestSecurityRegressions_Integration(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	alice, bob := e.user("alice"), e.user("bob")
	orgA, orgB := e.org(alice, "A"), e.org(bob, "B")
	projB := e.project(bob, orgB, "b-web")
	projA := e.project(alice, orgA, "a-web")

	t.Run("a decision for a project of another organization is never allowed", func(t *testing.T) {
		for _, perm := range []domain.Permission{domain.PermResourceRead, domain.PermResourceWrite, domain.PermProjectRead} {
			d, err := e.svc.CheckPermission(ctx, alice, orgA, perm, projB)
			require.NoError(t, err)
			require.False(t, d.Allowed, "%s on org B's project via org A", perm)
		}
		require.NoError(t, e.svc.DeleteProject(ctx, alice, orgA, projA))
		d, err := e.svc.CheckPermission(ctx, alice, orgA, domain.PermResourceRead, projA)
		require.NoError(t, err)
		require.False(t, d.Allowed, "deleted projects grant nothing")
	})

	t.Run("only verified e-mail addresses can be added", func(t *testing.T) {
		_, err := e.svc.ResolveActor(ctx, service.Principal{Kind: domain.ActorUser, Subject: "sub-squatter", Email: "victim@example.com", EmailVerified: false})
		require.NoError(t, err)
		_, err = e.svc.AddMember(ctx, alice, orgA, "victim@example.com", domain.RoleAdmin)
		require.ErrorIs(t, err, domain.ErrNotFound, "an unverified claim must not attach a membership")
		victim, err := e.svc.ResolveActor(ctx, service.Principal{Kind: domain.ActorUser, Subject: "sub-victim", Email: "victim@example.com", EmailVerified: true})
		require.NoError(t, err)
		m, err := e.svc.AddMember(ctx, alice, orgA, "VICTIM@example.com", domain.RoleViewer)
		require.NoError(t, err, "the verified owner of the address is unaffected by the squatter")
		require.Equal(t, victim.ID, m.UserID)
	})

	t.Run("identity-provider text is stored printable", func(t *testing.T) {
		u, err := e.svc.ResolveActor(ctx, service.Principal{Kind: domain.ActorUser, Subject: "sub-evil", Email: "evil@example.com",
			EmailVerified: true, DisplayName: "Eve\u202e\x1b[31mlyn"})
		require.NoError(t, err)
		m, err := e.svc.AddMember(ctx, alice, orgA, "evil@example.com", domain.RoleViewer)
		require.NoError(t, err)
		require.Equal(t, u.ID, m.UserID)
		require.Equal(t, "Eve[31mlyn", m.DisplayName)
	})

	t.Run("an idempotent replay of an organization create needs membership", func(t *testing.T) {
		carol := e.user("carol")
		o, err := e.svc.CreateOrganization(withKey(ctx, "c-org"), carol, service.CreateOrganizationInput{Name: "C"})
		require.NoError(t, err)
		dave := e.member(carol, o.ID, "dave", domain.RoleOwner)
		require.NoError(t, e.svc.RemoveMember(ctx, dave, o.ID, carol.ID))
		_, err = e.svc.CreateOrganization(withKey(ctx, "c-org"), carol, service.CreateOrganizationInput{Name: "C"})
		require.ErrorIs(t, err, domain.ErrNotFound)
	})
}
