//go:build integration

package service_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/zodixdev/infrapilot/services/control-plane/internal/domain"
	"github.com/zodixdev/infrapilot/services/control-plane/internal/service"
	"github.com/zodixdev/infrapilot/services/control-plane/internal/testdb"
)

type env struct {
	t   *testing.T
	db  *testdb.DB
	svc *service.Service
}

func newEnv(t *testing.T) *env {
	t.Helper()
	d := testdb.Start(t)
	svc, err := service.New(d.App)
	require.NoError(t, err)
	return &env{t: t, db: d, svc: svc}
}

// user signs a user in (creates the record) and returns the actor.
func (e *env) user(name string) service.Actor {
	e.t.Helper()
	a, err := e.svc.ResolveActor(e.t.Context(), service.Principal{Kind: domain.ActorUser, Subject: "sub-" + name,
		Email: name + "@example.com", EmailVerified: true, DisplayName: name})
	require.NoError(e.t, err)
	return a
}

func (e *env) org(owner service.Actor, name string) uuid.UUID {
	e.t.Helper()
	o, err := e.svc.CreateOrganization(e.t.Context(), owner, service.CreateOrganizationInput{Name: name})
	require.NoError(e.t, err)
	return o.ID
}

func (e *env) member(owner service.Actor, org uuid.UUID, name string, role domain.Role) service.Actor {
	e.t.Helper()
	a := e.user(name)
	_, err := e.svc.AddMember(e.t.Context(), owner, org, name+"@example.com", role)
	require.NoError(e.t, err)
	return a
}

func (e *env) project(owner service.Actor, org uuid.UUID, slug string) uuid.UUID {
	e.t.Helper()
	p, err := e.svc.CreateProject(e.t.Context(), owner, org, service.CreateProjectInput{Name: slug, Slug: slug})
	require.NoError(e.t, err)
	return p.ID
}

// key creates an API key and returns the actor it authenticates as.
func (e *env) key(owner service.Actor, org, project uuid.UUID, scopes ...domain.Scope) (service.Actor, string) {
	e.t.Helper()
	k, err := e.svc.CreateAPIKey(e.t.Context(), owner, org, service.CreateAPIKeyInput{ProjectID: project, Name: "key", Scopes: scopes})
	require.NoError(e.t, err)
	return service.Actor{Kind: domain.ActorAPIKey, ID: k.Key.ID}, k.Secret
}

func withKey(ctx context.Context, key string) context.Context {
	return service.WithRequestMeta(ctx, service.RequestMeta{RequestID: "req-" + key, IdempotencyKey: key})
}
