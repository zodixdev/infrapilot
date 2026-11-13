package health_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/zodixdev/infrapilot/pkg/health"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

func TestRegistry_GateAndChecks(t *testing.T) {
	t.Parallel()
	r := health.NewRegistry()
	require.False(t, r.Ready())
	require.Equal(t, health.StatusDown, r.Readiness(t.Context()).Status, "closed gate → down")

	r.SetReady(true)
	require.Equal(t, health.StatusUp, r.Readiness(t.Context()).Status)

	var dbUp atomic.Bool
	dbUp.Store(true)
	r.Register("postgres", func(context.Context) error {
		if dbUp.Load() {
			return nil
		}
		return errors.New("connection refused")
	})
	rep := r.Readiness(t.Context())
	require.Equal(t, health.StatusUp, rep.Status)
	require.Equal(t, health.StatusUp, rep.Checks["postgres"].Status)

	dbUp.Store(false)
	rep = r.Readiness(t.Context())
	require.Equal(t, health.StatusDown, rep.Status)
	require.Equal(t, "connection refused", rep.Checks["postgres"].Error)
	require.Equal(t, []string{"postgres"}, r.Names())
}

func TestRegistry_CheckTimeoutAndPanic(t *testing.T) {
	t.Parallel()
	r := health.NewRegistry(health.WithTimeout(20 * time.Millisecond))
	r.SetReady(true)
	r.Register("slow", func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})
	r.Register("panicky", func(context.Context) error { panic("boom") })

	rep := r.Readiness(t.Context())
	require.Equal(t, health.StatusDown, rep.Status)
	require.Equal(t, health.StatusDown, rep.Checks["slow"].Status)
	require.Contains(t, rep.Checks["slow"].Error, "deadline")
	require.Equal(t, "health check panicked", rep.Checks["panicky"].Error)
}

func TestRegistry_PeriodicCheckFlipsOnDependencyLoss(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	var healthy atomic.Bool
	healthy.Store(true)
	r := health.NewRegistry()
	r.SetReady(true)
	r.Register("dep", func(context.Context) error {
		calls.Add(1)
		if healthy.Load() {
			return nil
		}
		return errors.New("lost")
	}, health.Periodic(10*time.Millisecond))

	// Before Run: evaluated inline once, then cached.
	require.Equal(t, health.StatusUp, r.Readiness(t.Context()).Status)
	require.Equal(t, int32(1), calls.Load())
	require.Equal(t, health.StatusUp, r.Readiness(t.Context()).Status)
	require.Equal(t, int32(1), calls.Load(), "periodic checks are not re-run per request")

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	healthy.Store(false)
	require.Eventually(t, func() bool { return r.Readiness(t.Context()).Status == health.StatusDown }, time.Second, 5*time.Millisecond)
	healthy.Store(true)
	require.Eventually(t, func() bool { return r.Readiness(t.Context()).Status == health.StatusUp }, time.Second, 5*time.Millisecond)

	cancel()
	require.NoError(t, <-done)
}

func TestRegistry_RunWithoutPeriodicChecksWaitsForContext(t *testing.T) {
	t.Parallel()
	r := health.NewRegistry()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	cancel()
	require.NoError(t, <-done)
}

func TestHandlers(t *testing.T) {
	t.Parallel()
	r := health.NewRegistry()
	r.Register("x", func(context.Context) error { return errors.New("nope") })

	rec := httptest.NewRecorder()
	r.LivenessHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", http.NoBody))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "no-store", rec.Header().Get("Cache-Control"))

	rec = httptest.NewRecorder()
	r.ReadinessHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", http.NoBody))
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	var rep health.Report
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rep))
	require.Equal(t, health.StatusDown, rep.Status)
	require.Equal(t, "nope", rep.Checks["x"].Error)

	r.SetReady(true)
	r.Register("x", func(context.Context) error { return nil })
	rec = httptest.NewRecorder()
	r.ReadinessHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", http.NoBody))
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestResult_JSONDurationInMilliseconds(t *testing.T) {
	t.Parallel()
	b, err := json.Marshal(health.Result{Status: health.StatusUp, Duration: 1500 * time.Microsecond, CheckedAt: time.Unix(0, 0).UTC()})
	require.NoError(t, err)
	require.Contains(t, string(b), `"duration_ms":1.5`)
	require.NotContains(t, string(b), `"Duration"`)
}

// TestRegistry_ContextIgnoringCheckDoesNotPileUp: a check that ignores its context occupies at most one
// goroutine; later evaluations report it as still running (go-review M6).
func TestRegistry_ContextIgnoringCheckDoesNotPileUp(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	var started atomic.Int32
	r := health.NewRegistry(health.WithTimeout(10 * time.Millisecond))
	r.SetReady(true)
	r.Register("stuck", func(context.Context) error {
		started.Add(1)
		<-release // ignores ctx
		return nil
	})
	for range 20 {
		rep := r.Readiness(t.Context())
		require.Equal(t, health.StatusDown, rep.Status)
	}
	require.Equal(t, int32(1), started.Load(), "only one goroutine for the stuck check")
	require.Contains(t, r.Readiness(t.Context()).Checks["stuck"].Error, "still running")
	close(release)
	require.Eventually(t, func() bool { return r.Readiness(t.Context()).Status == health.StatusUp }, time.Second, 5*time.Millisecond)
}

func TestRegistry_ErrorsAreScrubbed(t *testing.T) {
	t.Parallel()
	r := health.NewRegistry()
	r.Register("db", func(context.Context) error { return errors.New("dial postgres://app:hunter2@db:5432/x: refused") })
	got := r.Readiness(t.Context()).Checks["db"].Error
	require.NotContains(t, got, "hunter2")
	require.Contains(t, got, "refused")
}
