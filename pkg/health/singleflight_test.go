package health_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zodixdev/infrapilot/pkg/health"
)

// TestRegistry_ConcurrentProbesShareOneEvaluation: overlapping /readyz requests on a healthy dependency all
// report up and run the check once (an overlapping probe must never report a false DOWN).
func TestRegistry_ConcurrentProbesShareOneEvaluation(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	release := make(chan struct{})
	r := health.NewRegistry(health.WithTimeout(2 * time.Second))
	r.SetReady(true)
	r.Register("db", func(context.Context) error {
		calls.Add(1)
		<-release
		return nil
	})
	var wg sync.WaitGroup
	statuses := make(chan health.Status, 10)
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			statuses <- r.Readiness(t.Context()).Status
		}()
	}
	require.Eventually(t, func() bool { return calls.Load() == 1 }, time.Second, time.Millisecond)
	time.Sleep(20 * time.Millisecond) // let the other callers join the flight
	close(release)
	wg.Wait()
	close(statuses)
	for s := range statuses {
		require.Equal(t, health.StatusUp, s)
	}
	require.Equal(t, int32(1), calls.Load(), "one evaluation shared by all callers")
}

// TestRegistry_HungCheckHoldsOneGoroutine: a check that ignores its context is not restarted while it hangs;
// callers get DOWN within the timeout and then "still running".
func TestRegistry_HungCheckHoldsOneGoroutine(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	release := make(chan struct{})
	r := health.NewRegistry(health.WithTimeout(20 * time.Millisecond))
	r.SetReady(true)
	r.Register("stuck", func(context.Context) error {
		calls.Add(1)
		<-release
		return errors.New("late")
	})
	start := time.Now()
	require.Equal(t, health.StatusDown, r.Readiness(t.Context()).Status)
	require.Less(t, time.Since(start), time.Second)
	time.Sleep(30 * time.Millisecond)
	for range 5 {
		rep := r.Readiness(t.Context())
		require.Contains(t, rep.Checks["stuck"].Error, "still running")
	}
	require.Equal(t, int32(1), calls.Load())
	close(release)
	require.Eventually(t, func() bool { return calls.Load() == 2 || r.Readiness(t.Context()).Checks["stuck"].Error == "late" },
		time.Second, 5*time.Millisecond)
}

func TestRegistry_LongErrorsTruncatedOnRuneBoundary(t *testing.T) {
	t.Parallel()
	r := health.NewRegistry()
	r.Register("x", func(context.Context) error { return errors.New(strings.Repeat("é", 400)) })
	msg := r.Readiness(t.Context()).Checks["x"].Error
	require.LessOrEqual(t, len(msg), 512+len("…"))
	require.True(t, strings.HasSuffix(msg, "…"))
	require.NotContains(t, msg, "�")
}
