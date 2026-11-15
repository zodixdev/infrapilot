package authz_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/metric/noop"
	"go.uber.org/goleak"
	"google.golang.org/grpc"
	"google.golang.org/grpc/status"

	cpv1 "github.com/zodixdev/infrapilot/gen/go/infrapilot/controlplane/v1"
	"github.com/zodixdev/infrapilot/services/api-gateway/internal/authz"
	"github.com/zodixdev/infrapilot/services/api-gateway/internal/principal"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

type memCache struct {
	mu   sync.Mutex
	vals map[string][]byte
	ttls map[string]time.Duration
	down bool
}

func (c *memCache) Get(_ context.Context, key string) ([]byte, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.down {
		return nil, false, errors.New("redis down")
	}
	v, ok := c.vals[key]
	return v, ok, nil
}

func (c *memCache) Set(_ context.Context, key string, val []byte, ttl time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.down {
		return errors.New("redis down")
	}
	c.vals[key], c.ttls[key] = val, ttl
	return nil
}

type fakeClient struct {
	cpv1.AuthorizationServiceClient
	calls   atomic.Int32
	allowed bool
	role    cpv1.Role
	release chan struct{}
}

func (f *fakeClient) CheckPermission(ctx context.Context, _ *cpv1.CheckPermissionRequest, _ ...grpc.CallOption) (*cpv1.CheckPermissionResponse, error) {
	f.calls.Add(1)
	if f.release != nil {
		select {
		case <-f.release:
		case <-ctx.Done():
			return nil, status.FromContextError(ctx.Err()).Err()
		}
	}
	return &cpv1.CheckPermissionResponse{Allowed: f.allowed, Role: f.role}, nil
}

func setup(t *testing.T, f *fakeClient) (*authz.Authorizer, *memCache) {
	t.Helper()
	c := &memCache{vals: map[string][]byte{}, ttls: map[string]time.Duration{}}
	a, err := authz.New(c, f, noop.NewMeterProvider().Meter("t"), 5*time.Second)
	require.NoError(t, err)
	return a, c
}

var (
	alice = &principal.Principal{Kind: principal.User, Subject: "alice"}
	bob   = &principal.Principal{Kind: principal.User, Subject: "bob"}
)

const org, project = "0192f7a0-3c1e-7c9a-9d41-9a1f0f3b2c11", "0192f7a0-3c1e-7c9a-9d41-9a1f0f3b2c22"

func TestCheck(t *testing.T) {
	t.Parallel()
	t.Run("allow cached 30 s, deny 5 s", func(t *testing.T) {
		t.Parallel()
		f := &fakeClient{allowed: true, role: cpv1.Role_ROLE_ADMIN}
		a, c := setup(t, f)
		for range 3 {
			d, err := a.Check(t.Context(), alice, org, cpv1.Permission_PERMISSION_ORG_READ, "")
			require.NoError(t, err)
			require.Equal(t, authz.Decision{Allowed: true, Member: true}, d)
		}
		require.EqualValues(t, 1, f.calls.Load())
		f.allowed, f.role = false, cpv1.Role_ROLE_UNSPECIFIED
		d, err := a.Check(t.Context(), alice, org, cpv1.Permission_PERMISSION_ORG_DELETE, "")
		require.NoError(t, err)
		require.Equal(t, authz.Decision{}, d, "non-member denial")
		ttls := map[time.Duration]int{}
		for _, ttl := range c.ttls {
			ttls[ttl]++
		}
		require.Equal(t, map[time.Duration]int{30 * time.Second: 1, 5 * time.Second: 1}, ttls)
	})

	t.Run("decisions are keyed by principal, org, permission and project", func(t *testing.T) {
		t.Parallel()
		f := &fakeClient{allowed: true, role: cpv1.Role_ROLE_VIEWER}
		a, c := setup(t, f)
		ctx := t.Context()
		_, _ = a.Check(ctx, alice, org, cpv1.Permission_PERMISSION_RESOURCE_READ, project)
		_, _ = a.Check(ctx, bob, org, cpv1.Permission_PERMISSION_RESOURCE_READ, project)
		_, _ = a.Check(ctx, alice, org, cpv1.Permission_PERMISSION_RESOURCE_WRITE, project)
		_, _ = a.Check(ctx, alice, org, cpv1.Permission_PERMISSION_RESOURCE_READ, "")
		require.Len(t, c.vals, 4)
		require.EqualValues(t, 4, f.calls.Load())
	})

	t.Run("Redis down: every decision comes from control-plane (F03)", func(t *testing.T) {
		t.Parallel()
		f := &fakeClient{allowed: true, role: cpv1.Role_ROLE_OWNER}
		a, c := setup(t, f)
		c.down = true
		for range 3 {
			d, err := a.Check(t.Context(), alice, org, cpv1.Permission_PERMISSION_ORG_READ, "")
			require.NoError(t, err)
			require.True(t, d.Allowed)
		}
		require.EqualValues(t, 3, f.calls.Load())
	})

	t.Run("concurrent misses share one call", func(t *testing.T) {
		t.Parallel()
		f := &fakeClient{allowed: true, role: cpv1.Role_ROLE_OWNER, release: make(chan struct{})}
		a, _ := setup(t, f)
		var wg sync.WaitGroup
		for range 10 {
			wg.Go(func() {
				_, err := a.Check(t.Context(), alice, org, cpv1.Permission_PERMISSION_ORG_READ, "")
				require.NoError(t, err)
			})
		}
		require.Eventually(t, func() bool { return f.calls.Load() == 1 }, time.Second, time.Millisecond)
		close(f.release)
		wg.Wait()
		require.EqualValues(t, 1, f.calls.Load())
	})

	t.Run("the first caller's deadline does not cancel the shared RPC", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			f := &fakeClient{allowed: true, role: cpv1.Role_ROLE_OWNER, release: make(chan struct{})}
			a, _ := setup(t, f)
			short, cancelShort := context.WithTimeout(t.Context(), time.Second)
			defer cancelShort()
			shortErr := make(chan error, 1)
			go func() {
				_, err := a.Check(short, alice, org, cpv1.Permission_PERMISSION_ORG_READ, "")
				shortErr <- err
			}()
			synctest.Wait()
			require.EqualValues(t, 1, f.calls.Load())

			long, cancelLong := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancelLong()
			type result struct {
				decision authz.Decision
				err      error
			}
			longResult := make(chan result, 1)
			go func() {
				d, err := a.Check(long, alice, org, cpv1.Permission_PERMISSION_ORG_READ, "")
				longResult <- result{decision: d, err: err}
			}()
			synctest.Wait()

			require.ErrorIs(t, <-shortErr, context.DeadlineExceeded)
			synctest.Wait() // Let any cancellation of the shared RPC reach the second waiter before releasing it.
			close(f.release)
			got := <-longResult
			require.NoError(t, got.err)
			require.Equal(t, authz.Decision{Allowed: true, Member: true}, got.decision)
			require.EqualValues(t, 1, f.calls.Load(), "both callers used the same RPC")
		})
	})
}
