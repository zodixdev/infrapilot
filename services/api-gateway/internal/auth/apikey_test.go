package auth_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/metric/noop"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	cpv1 "github.com/zodixdev/infrapilot/gen/go/infrapilot/controlplane/v1"
	"github.com/zodixdev/infrapilot/services/api-gateway/internal/auth"
	"github.com/zodixdev/infrapilot/services/api-gateway/internal/principal"
	"github.com/zodixdev/infrapilot/services/api-gateway/internal/redisx"
)

// memCache is a fake of the Cache port.
type memCache struct {
	mu   sync.Mutex
	vals map[string][]byte
	ttls map[string]time.Duration
	err  error
}

func newMemCache() *memCache {
	return &memCache{vals: map[string][]byte{}, ttls: map[string]time.Duration{}}
}

func (c *memCache) Get(_ context.Context, key string) ([]byte, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return nil, false, c.err
	}
	v, ok := c.vals[key]
	return v, ok, nil
}

func (c *memCache) Set(_ context.Context, key string, val []byte, ttl time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	c.vals[key], c.ttls[key] = val, ttl
	return nil
}

// fakeAuthz is a fake of the generated AuthorizationService client port.
type fakeAuthz struct {
	cpv1.AuthorizationServiceClient
	calls   atomic.Int32
	resp    *cpv1.AuthenticateApiKeyResponse
	err     error
	release chan struct{}
}

func (f *fakeAuthz) AuthenticateApiKey(context.Context, *cpv1.AuthenticateApiKeyRequest, ...grpc.CallOption) (*cpv1.AuthenticateApiKeyResponse, error) { //nolint:revive // name fixed by the generated client interface
	f.calls.Add(1)
	if f.release != nil {
		<-f.release
	}
	return f.resp, f.err
}

func newKey(t *testing.T) string {
	t.Helper()
	b := make([]byte, 32)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return "ipk_" + base64.RawURLEncoding.EncodeToString(b)
}

func resolver(t *testing.T, c auth.Cache, f *fakeAuthz) *auth.KeyResolver {
	t.Helper()
	r, err := auth.NewKeyResolver(c, f, noop.NewMeterProvider().Meter("t"))
	require.NoError(t, err)
	return r
}

func okResp() *cpv1.AuthenticateApiKeyResponse {
	return &cpv1.AuthenticateApiKeyResponse{ApiKeyId: "k1", OrganizationId: "o1", ProjectId: "p1",
		Scopes: []cpv1.ApiKeyScope{cpv1.ApiKeyScope_API_KEY_SCOPE_READ}}
}

func TestKeyResolver(t *testing.T) {
	t.Parallel()
	t.Run("garbage never reaches control-plane", func(t *testing.T) {
		t.Parallel()
		f := &fakeAuthz{resp: okResp()}
		r := resolver(t, newMemCache(), f)
		for _, k := range []string{"", "ipk_short", "Bearer x", "ipk_" + string(make([]byte, 43))} {
			_, err := r.Resolve(t.Context(), k)
			require.ErrorIs(t, err, auth.ErrInvalidKey)
		}
		require.Zero(t, f.calls.Load())
	})

	t.Run("miss resolves and caches; hit does not call", func(t *testing.T) {
		t.Parallel()
		c, f := newMemCache(), &fakeAuthz{resp: okResp()}
		r := resolver(t, c, f)
		k := newKey(t)
		for range 3 {
			p, err := r.Resolve(t.Context(), k)
			require.NoError(t, err)
			require.Equal(t, principal.APIKey, p.Kind)
			require.Equal(t, "o1", p.OrgID)
		}
		require.EqualValues(t, 1, f.calls.Load())
		for key, ttl := range c.ttls {
			require.NotContains(t, key, k, "cache keys never contain the key itself")
			require.Equal(t, 30*time.Second, ttl)
		}
	})

	t.Run("unknown keys are cached briefly", func(t *testing.T) {
		t.Parallel()
		c, f := newMemCache(), &fakeAuthz{err: status.Error(codes.Unauthenticated, "no")}
		r := resolver(t, c, f)
		k := newKey(t)
		for range 2 {
			_, err := r.Resolve(t.Context(), k)
			require.ErrorIs(t, err, auth.ErrInvalidKey)
		}
		require.EqualValues(t, 1, f.calls.Load())
		for _, ttl := range c.ttls {
			require.Equal(t, 5*time.Second, ttl)
		}
	})

	t.Run("expired keys are refused even from cache", func(t *testing.T) {
		t.Parallel()
		resp := okResp()
		resp.ExpireTime = timestamppb.New(time.Now().Add(-time.Second))
		r := resolver(t, newMemCache(), &fakeAuthz{resp: resp})
		_, err := r.Resolve(t.Context(), newKey(t))
		require.ErrorIs(t, err, auth.ErrInvalidKey)
	})

	t.Run("backend outage is reported, not cached", func(t *testing.T) {
		t.Parallel()
		c, f := newMemCache(), &fakeAuthz{err: status.Error(codes.Unavailable, "down")}
		r := resolver(t, c, f)
		_, err := r.Resolve(t.Context(), newKey(t))
		require.ErrorIs(t, err, auth.ErrBackendUnavailable)
		require.Empty(t, c.vals)
	})

	t.Run("tampered or unreadable cache entries fall back to control-plane", func(t *testing.T) {
		t.Parallel()
		c, f := newMemCache(), &fakeAuthz{resp: okResp()}
		c.err = redisx.ErrTampered
		r := resolver(t, c, f)
		_, err := r.Resolve(t.Context(), newKey(t))
		require.NoError(t, err)
		require.EqualValues(t, 1, f.calls.Load())
	})

	t.Run("concurrent misses share one call; a cancelled waiter returns early", func(t *testing.T) {
		t.Parallel()
		f := &fakeAuthz{resp: okResp(), release: make(chan struct{})}
		r := resolver(t, newMemCache(), f)
		k := newKey(t)
		var wg sync.WaitGroup
		for range 10 {
			wg.Go(func() {
				_, err := r.Resolve(t.Context(), k)
				require.NoError(t, err)
			})
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := r.Resolve(ctx, k)
		require.ErrorIs(t, err, context.Canceled)
		require.Eventually(t, func() bool { return f.calls.Load() == 1 }, time.Second, time.Millisecond)
		close(f.release)
		wg.Wait()
		require.EqualValues(t, 1, f.calls.Load())
	})
}
