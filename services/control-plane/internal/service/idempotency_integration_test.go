//go:build integration

package service_test

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/zodixdev/infrapilot/services/control-plane/internal/domain"
	"github.com/zodixdev/infrapilot/services/control-plane/internal/service"
)

// TestIdempotency_OverlappingDuplicates_Integration makes the overlap certain: many transactions with the same key
// and the same natural unique key (slug, resource name) start together. Exactly one resource is created; every
// other request either replays it or is told the original is still in progress (never a natural-key conflict), and
// a retry after the original committed replays it.
func TestIdempotency_OverlappingDuplicates_Integration(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	alice := e.user("alice")
	org := e.org(alice, "Acme")
	project := e.project(alice, org, "web")

	run := func(n int, fn func() (uuid.UUID, error)) uuid.UUID {
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
		created := uuid.Nil
		for i := range n {
			if errs[i] != nil {
				require.ErrorIs(t, errs[i], domain.ErrConflict)
				require.Contains(t, errs[i].Error(), "in progress", "never a collision on the natural key")
				continue
			}
			if created == uuid.Nil {
				created = ids[i]
			}
			require.Equal(t, created, ids[i], "a single resource")
		}
		require.NotEqual(t, uuid.Nil, created)
		replay, err := fn()
		require.NoError(t, err, "after the original committed, a retry replays it")
		require.Equal(t, created, replay)
		return created
	}
	for round := range 5 {
		key := "proj-" + string(rune('a'+round))
		slug := "race-" + string(rune('a'+round))
		run(30, func() (uuid.UUID, error) {
			p, err := e.svc.CreateProject(withKey(ctx, key), alice, org, service.CreateProjectInput{Name: "Race", Slug: slug})
			return p.ID, err
		})
	}
	run(30, func() (uuid.UUID, error) {
		r, err := e.svc.CreateResource(withKey(ctx, "res-1"), alice, org, project, service.CreateResourceInput{Kind: domain.KindHost, Name: "db"})
		return r.ID, err
	})
	run(30, func() (uuid.UUID, error) {
		o, err := e.svc.CreateOrganization(withKey(ctx, "org-race"), alice, service.CreateOrganizationInput{Name: "Race"})
		return o.ID, err
	})
}

// TestIdempotency_ExpiredRecordIsReplaced_Integration: after the 24 h window a key starts a new create even before the
// hourly purge removed the old record (no 500, no stale replay).
func TestIdempotency_ExpiredRecordIsReplaced_Integration(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	alice := e.user("alice")
	org := e.org(alice, "Acme")
	first, err := e.svc.CreateProject(withKey(ctx, "old"), alice, org, service.CreateProjectInput{Name: "A", Slug: "aaa"})
	require.NoError(t, err)
	o1, err := e.svc.CreateOrganization(withKey(ctx, "old-org"), alice, service.CreateOrganizationInput{Name: "Old"})
	require.NoError(t, err)

	su, err := pgx.Connect(ctx, e.db.SuperuserURL)
	require.NoError(t, err)
	defer func() { _ = su.Close(context.WithoutCancel(ctx)) }()
	for _, q := range []string{
		`UPDATE idempotency_keys SET created_at = now() - interval '25 hours'`,
		`UPDATE org_create_keys SET created_at = now() - interval '25 hours'`,
	} {
		_, execErr := su.Exec(ctx, q)
		require.NoError(t, execErr)
	}
	second, err := e.svc.CreateProject(withKey(ctx, "old"), alice, org, service.CreateProjectInput{Name: "B", Slug: "bbb"})
	require.NoError(t, err, "an expired key is a new request, even with a different body")
	require.NotEqual(t, first.ID, second.ID)
	o2, err := e.svc.CreateOrganization(withKey(ctx, "old-org"), alice, service.CreateOrganizationInput{Name: "New"})
	require.NoError(t, err)
	require.NotEqual(t, o1.ID, o2.ID)
	replay, err := e.svc.CreateProject(withKey(ctx, "old"), alice, org, service.CreateProjectInput{Name: "B", Slug: "bbb"})
	require.NoError(t, err)
	require.Equal(t, second.ID, replay.ID, "the replaced record is live again")
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
