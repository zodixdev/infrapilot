//go:build integration

package service_test

import (
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/zodixdev/infrapilot/services/control-plane/internal/domain"
	"github.com/zodixdev/infrapilot/services/control-plane/internal/service"
)

// TestIdempotency_OverlappingDuplicates_Integration makes the overlap certain: many transactions with the same key
// and the same natural unique key (slug, resource name) start together. Without the advisory lock the losers
// collide on the natural key (409); with it they wait and replay the winner's resource.
func TestIdempotency_OverlappingDuplicates_Integration(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	alice := e.user("alice")
	org := e.org(alice, "Acme")
	project := e.project(alice, org, "web")

	run := func(n int, fn func() (uuid.UUID, error)) []uuid.UUID {
		start := make(chan struct{})
		ids := make([]uuid.UUID, n)
		errs := make([]error, n)
		var wg sync.WaitGroup
		for i := range n {
			wg.Go(func() {
				<-start
				ids[i], errs[i] = fn()
			})
		}
		close(start)
		wg.Wait()
		for _, err := range errs {
			require.NoError(t, err)
		}
		return ids
	}
	for round := range 5 {
		key := "proj-" + string(rune('a'+round))
		slug := "race-" + string(rune('a'+round))
		ids := run(30, func() (uuid.UUID, error) {
			p, err := e.svc.CreateProject(withKey(ctx, key), alice, org, service.CreateProjectInput{Name: "Race", Slug: slug})
			return p.ID, err
		})
		for _, id := range ids {
			require.Equal(t, ids[0], id, "every duplicate replays the single created project")
		}
	}
	ids := run(30, func() (uuid.UUID, error) {
		r, err := e.svc.CreateResource(withKey(ctx, "res-1"), alice, org, project, service.CreateResourceInput{Kind: domain.KindHost, Name: "db"})
		return r.ID, err
	})
	for _, id := range ids {
		require.Equal(t, ids[0], id)
	}
	orgIDs := run(30, func() (uuid.UUID, error) {
		o, err := e.svc.CreateOrganization(withKey(ctx, "org-race"), alice, service.CreateOrganizationInput{Name: "Race"})
		return o.ID, err
	})
	for _, id := range orgIDs {
		require.Equal(t, orgIDs[0], id)
	}
}

// TestIdempotency_ReplayAcrossRestart_Integration: the dedup state lives in the database only — a fresh service
// instance (as after a restart) replays the original resource.
func TestIdempotency_ReplayAcrossRestart_Integration(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	alice := e.user("alice")
	org := e.org(alice, "Acme")
	first, err := e.svc.CreateProject(withKey(ctx, "k"), alice, org, service.CreateProjectInput{Name: "Web", Slug: "web"})
	require.NoError(t, err)
	o1, err := e.svc.CreateOrganization(withKey(ctx, "o"), alice, service.CreateOrganizationInput{Name: "Beta"})
	require.NoError(t, err)

	restarted, err := service.New(e.db.App)
	require.NoError(t, err)
	again, err := restarted.CreateProject(withKey(ctx, "k"), alice, org, service.CreateProjectInput{Name: "Web", Slug: "web"})
	require.NoError(t, err)
	require.Equal(t, first.ID, again.ID)
	o2, err := restarted.CreateOrganization(withKey(ctx, "o"), alice, service.CreateOrganizationInput{Name: "Beta"})
	require.NoError(t, err)
	require.Equal(t, o1.ID, o2.ID)
}
