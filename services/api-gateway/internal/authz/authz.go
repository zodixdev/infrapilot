// Package authz pre-authorizes org-scoped requests at the edge through control-plane's CheckPermission, with
// decisions cached in Redis (allow 30 s, deny 5 s) and a direct gRPC fallback when Redis is unavailable (ADR-0022,
// ADR-0026, F03). Owning services re-check authoritatively.
package authz

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	cpv1 "github.com/zodixdev/infrapilot/gen/go/infrapilot/controlplane/v1"
	"github.com/zodixdev/infrapilot/services/api-gateway/internal/auth"
	"github.com/zodixdev/infrapilot/services/api-gateway/internal/principal"
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
	lookups  metric.Int64Counter
}

// New returns an authorizer.
func New(cache auth.Cache, client cpv1.AuthorizationServiceClient, meter metric.Meter) (*Authorizer, error) {
	c, err := meter.Int64Counter("gateway_cache_requests_total", metric.WithDescription("Edge cache lookups by cache and result"))
	if err != nil {
		return nil, fmt.Errorf("authz: %w", err)
	}
	return &Authorizer{cache: cache, client: client, AllowTTL: 30 * time.Second, DenyTTL: 5 * time.Second, lookups: c}, nil
}

// Check decides perm for the principal in ctx (which Outgoing forwards) on org and, optionally, project.
func (a *Authorizer) Check(ctx context.Context, p *principal.Principal, org string, perm cpv1.Permission, project string) (Decision, error) {
	sum := sha256.Sum256([]byte(strings.Join([]string{p.Key(), org, perm.String(), project}, "|")))
	key := "pc:" + hex.EncodeToString(sum[:])
	if b, found, err := a.cache.Get(ctx, key); err == nil && found && len(b) == 2 {
		a.count(ctx, "hit")
		return Decision{Allowed: b[0] == '1', Member: b[1] == '1'}, nil
	} else if err != nil {
		a.count(ctx, "error") // F03: decide through control-plane
	} else {
		a.count(ctx, "miss")
	}
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

// ErrDenied is returned by callers that turn a negative decision into an error.
var ErrDenied = errors.New("authz: denied")
