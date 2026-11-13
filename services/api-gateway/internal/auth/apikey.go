package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"golang.org/x/sync/singleflight"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	cpv1 "github.com/zodixdev/infrapilot/gen/go/infrapilot/controlplane/v1"
	"github.com/zodixdev/infrapilot/services/api-gateway/internal/principal"
	"github.com/zodixdev/infrapilot/services/api-gateway/internal/redisx"
)

// ErrInvalidKey is returned for unknown, revoked, expired or malformed API keys.
var ErrInvalidKey = errors.New("auth: invalid api key")

// ErrBackendUnavailable is returned when a key cannot be resolved now (control-plane unreachable); answer 503.
var ErrBackendUnavailable = errors.New("auth: authentication backend unavailable")

// Cache is the subset of redisx.Store the resolvers use.
type Cache interface {
	Get(ctx context.Context, key string) ([]byte, bool, error)
	Set(ctx context.Context, key string, val []byte, ttl time.Duration) error
}

// KeyResolver resolves API keys through control-plane with a short-lived cache keyed by the key's SHA-256
// (never the key itself). Revocations reach the edge within PositiveTTL (ADR-0026).
type KeyResolver struct {
	cache       Cache
	client      cpv1.AuthorizationServiceClient
	PositiveTTL time.Duration
	NegativeTTL time.Duration
	now         func() time.Time
	lookups     metric.Int64Counter
	sf          singleflight.Group
}

// NewKeyResolver returns a resolver with 30 s positive and 5 s negative caching.
func NewKeyResolver(cache Cache, client cpv1.AuthorizationServiceClient, meter metric.Meter) (*KeyResolver, error) {
	c, err := meter.Int64Counter("gateway_cache_requests_total", metric.WithDescription("Edge cache lookups by cache and result"))
	if err != nil {
		return nil, fmt.Errorf("auth: %w", err)
	}
	return &KeyResolver{cache: cache, client: client, PositiveTTL: 30 * time.Second, NegativeTTL: 5 * time.Second, now: time.Now, lookups: c}, nil
}

type cachedKey struct {
	Invalid   bool               `json:"invalid,omitempty"`
	KeyID     string             `json:"key_id,omitempty"`
	OrgID     string             `json:"org_id,omitempty"`
	ProjectID string             `json:"project_id,omitempty"`
	Scopes    []cpv1.ApiKeyScope `json:"scopes,omitempty"`
	ExpiresAt *time.Time         `json:"expires_at,omitempty"`
}

// looksLikeKey: "ipk_" + 43 base64url characters (32 random bytes). Garbage is rejected before any lookup.
func looksLikeKey(s string) bool {
	rest, ok := strings.CutPrefix(s, "ipk_")
	if !ok || len(rest) != 43 {
		return false
	}
	_, err := base64.RawURLEncoding.DecodeString(rest)
	return err == nil
}

// Resolve authenticates a raw API key.
func (r *KeyResolver) Resolve(ctx context.Context, raw string) (*principal.Principal, error) {
	if !looksLikeKey(raw) {
		return nil, ErrInvalidKey
	}
	sum := sha256.Sum256([]byte(raw))
	cacheKey := "ak:" + hex.EncodeToString(sum[:])
	b, found, err := r.cache.Get(ctx, cacheKey)
	switch {
	case errors.Is(err, redisx.ErrTampered):
		r.count(ctx, "corrupt")
	case err != nil:
		r.count(ctx, "error") // Redis down: resolve through control-plane (F03)
	case !found:
		r.count(ctx, "miss")
	default:
		var c cachedKey
		if json.Unmarshal(b, &c) == nil {
			r.count(ctx, "hit")
			return r.toPrincipal(&c)
		}
		r.count(ctx, "corrupt")
	}
	// Concurrent misses for the same key share one control-plane call; each waiter honors its own cancellation.
	ch := r.sf.DoChan(cacheKey, func() (any, error) {
		dctx, cancel := principal.Detach(ctx)
		defer cancel()
		return r.lookup(dctx, cacheKey, sum[:])
	})
	select {
	case res := <-ch:
		if res.Err != nil && errors.Is(res.Err, context.DeadlineExceeded) && ctx.Err() == nil {
			// The shared call ran under another waiter's (shorter) deadline; this request still has time left.
			c, err := r.lookup(ctx, cacheKey, sum[:])
			if err != nil {
				return nil, err
			}
			return r.toPrincipal(c)
		}
		if res.Err != nil {
			return nil, res.Err
		}
		c, _ := res.Val.(*cachedKey)
		return r.toPrincipal(c)
	case <-ctx.Done():
		return nil, fmt.Errorf("auth: %w", ctx.Err())
	}
}

// lookup asks control-plane and caches the answer (negative answers briefly).
func (r *KeyResolver) lookup(ctx context.Context, cacheKey string, sum []byte) (*cachedKey, error) {
	octx, err := principal.Outgoing(ctx) // request id only: there is no principal yet
	if err != nil {
		return nil, err
	}
	resp, err := r.client.AuthenticateApiKey(octx, &cpv1.AuthenticateApiKeyRequest{KeySha256: sum})
	var c cachedKey
	switch status.Code(err) {
	case codes.OK:
		c = cachedKey{KeyID: resp.GetApiKeyId(), OrgID: resp.GetOrganizationId(), ProjectID: resp.GetProjectId(), Scopes: resp.GetScopes()}
		if resp.GetExpireTime() != nil {
			t := resp.GetExpireTime().AsTime()
			c.ExpiresAt = &t
		}
	case codes.Unauthenticated:
		c = cachedKey{Invalid: true}
	default:
		return nil, fmt.Errorf("%w: %w", ErrBackendUnavailable, err)
	}
	ttl := r.PositiveTTL
	if c.Invalid {
		ttl = r.NegativeTTL
	}
	if b, mErr := json.Marshal(c); mErr == nil {
		_ = r.cache.Set(ctx, cacheKey, b, ttl) // best effort
	}
	return &c, nil
}

func (r *KeyResolver) toPrincipal(c *cachedKey) (*principal.Principal, error) {
	if c.Invalid || c.KeyID == "" || (c.ExpiresAt != nil && !c.ExpiresAt.After(r.now())) {
		return nil, ErrInvalidKey
	}
	return &principal.Principal{Kind: principal.APIKey, KeyID: c.KeyID, OrgID: c.OrgID, ProjectID: c.ProjectID, Scopes: c.Scopes}, nil
}

func (r *KeyResolver) count(ctx context.Context, result string) {
	r.lookups.Add(ctx, 1, metric.WithAttributes(attribute.String("cache", "apikey"), attribute.String("result", result)))
}

var (
	_ Cache = (*redisx.Store)(nil)
	_ Cache = (*redisx.SignedCache)(nil)
)
