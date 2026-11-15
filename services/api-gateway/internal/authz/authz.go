// Package authz pre-authorizes org-scoped requests at the edge through control-plane's CheckPermission, with
// decisions cached in Redis (allow 30 s, deny 5 s) and a direct gRPC fallback when Redis is unavailable (ADR-0022,
// ADR-0026, F03). Owning services re-check authoritatively.
package authz

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"golang.org/x/sync/singleflight"

	cpv1 "github.com/zodixdev/infrapilot/gen/go/infrapilot/controlplane/v1"
	"github.com/zodixdev/infrapilot/services/api-gateway/internal/auth"
	"github.com/zodixdev/infrapilot/services/api-gateway/internal/principal"
	"github.com/zodixdev/infrapilot/services/api-gateway/internal/redisx"
)

// Decision of a pre-authorization.
type Decision struct {
	Allowed bool
	// Member is true when a user is a member of the organization (denials of members are 403, of outsiders 404).
	Member bool
}

// Authorizer caches CheckPermission decisions.
type Authorizer struct {
	cache    auth.Cache
	client   cpv1.AuthorizationServiceClient
	AllowTTL time.Duration
	DenyTTL  time.Duration
	timeout  time.Duration
	lookups  metric.Int64Counter
	sf       singleflight.Group
}

// New returns an authorizer.
func New(cache auth.Cache, client cpv1.AuthorizationServiceClient, meter metric.Meter, timeout time.Duration) (*Authorizer, error) {
	c, err := meter.Int64Counter("gateway_cache_requests_total", metric.WithDescription("Edge cache lookups by cache and result"))
	if err != nil {
		return nil, fmt.Errorf("authz: %w", err)
	}
	return &Authorizer{cache: cache, client: client, AllowTTL: 30 * time.Second, DenyTTL: 5 * time.Second, timeout: timeout, lookups: c}, nil
}

// cacheKey hashes the length-prefixed parts, so no combination of values can collide with another.
func cacheKey(parts ...string) string {
	h := sha256.New()
	var n [8]byte
	for _, p := range parts {
		binary.BigEndian.PutUint64(n[:], uint64(len(p)))
		_, _ = h.Write(n[:]) // hash.Hash.Write never returns an error
		_, _ = h.Write([]byte(p))
	}
	return "pc:" + hex.EncodeToString(h.Sum(nil))
}

// Check decides perm for the principal in ctx (which Outgoing forwards) on org and, optionally, project. Org and
// project must be canonical UUID strings.
func (a *Authorizer) Check(ctx context.Context, p *principal.Principal, org string, perm cpv1.Permission, project string) (Decision, error) {
	key := cacheKey(p.Key(), org, perm.String(), project)
	b, found, err := a.cache.Get(ctx, key)
	switch {
	case errors.Is(err, redisx.ErrTampered):
		a.count(ctx, "corrupt")
	case err != nil:
		a.count(ctx, "error") // F03: decide through control-plane
	case found && len(b) == 2:
		a.count(ctx, "hit")
		return Decision{Allowed: b[0] == '1', Member: b[1] == '1'}, nil
	case found:
		a.count(ctx, "corrupt")
	default:
		a.count(ctx, "miss")
	}
	// The lookup keeps the first caller's metadata (the principal is part of the key) and has its own timeout.
	// Each caller waits on its own context.
	ch := a.sf.DoChan(key, func() (any, error) {
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), a.timeout)
		defer cancel()
		// A caller that missed the cache just before an earlier shared call finished starts a new one: the decision
		// is cached by then.
		if b, found, err := a.cache.Get(dctx, key); err == nil && found && len(b) == 2 {
			return Decision{Allowed: b[0] == '1', Member: b[1] == '1'}, nil
		}
		return a.decide(dctx, key, org, perm, project)
	})
	select {
	case res := <-ch:
		if res.Err != nil {
			return Decision{}, res.Err
		}
		d, _ := res.Val.(Decision)
		return d, nil
	case <-ctx.Done():
		return Decision{}, ctx.Err()
	}
}

func (a *Authorizer) decide(ctx context.Context, key, org string, perm cpv1.Permission, project string) (Decision, error) {
	octx, err := principal.Outgoing(ctx)
	if err != nil {
		return Decision{}, err
	}
	resp, err := a.client.CheckPermission(octx, &cpv1.CheckPermissionRequest{OrganizationId: org, Permission: perm, ProjectId: project})
	if err != nil {
		return Decision{}, err
	}
	d := Decision{Allowed: resp.GetAllowed(), Member: resp.GetRole() != cpv1.Role_ROLE_UNSPECIFIED}
	ttl := a.AllowTTL
	if !d.Allowed {
		ttl = a.DenyTTL
	}
	_ = a.cache.Set(ctx, key, []byte{flag(d.Allowed), flag(d.Member)}, ttl) // best effort
	return d, nil
}

func flag(b bool) byte {
	if b {
		return '1'
	}
	return '0'
}

func (a *Authorizer) count(ctx context.Context, result string) {
	a.lookups.Add(ctx, 1, metric.WithAttributes(attribute.String("cache", "authz"), attribute.String("result", result)))
}
