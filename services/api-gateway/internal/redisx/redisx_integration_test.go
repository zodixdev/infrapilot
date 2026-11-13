//go:build integration

package redisx_test

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/zodixdev/infrapilot/services/api-gateway/internal/redisx"
	"github.com/zodixdev/infrapilot/services/api-gateway/internal/testenv"
)

func TestGCRA_Integration(t *testing.T) {
	r := testenv.StartRedis(t)
	s, err := redisx.New(r.Client(t), redisx.Config{CallTimeout: 500 * time.Millisecond}, noop.NewMeterProvider().Meter("t"))
	require.NoError(t, err)
	ctx := t.Context()

	t.Run("burst is honored exactly under concurrency", func(t *testing.T) {
		var allowed atomic.Int32
		var wg sync.WaitGroup
		for range 100 {
			wg.Go(func() {
				d, err := s.Allow(ctx, "burst", 1, 10)
				require.NoError(t, err)
				if d.Allowed {
					allowed.Add(1)
				}
			})
		}
		wg.Wait()
		require.EqualValues(t, 10, allowed.Load(), "a slow rate admits exactly the burst")
	})

	t.Run("denials carry retry-after and the bucket refills", func(t *testing.T) {
		for i := range 5 {
			d, err := s.Allow(ctx, "refill", 20, 5)
			require.NoError(t, err)
			require.True(t, d.Allowed)
			require.Equal(t, 4-i, d.Remaining)
		}
		d, err := s.Allow(ctx, "refill", 20, 5)
		require.NoError(t, err)
		require.False(t, d.Allowed)
		require.Greater(t, d.RetryAfter, time.Duration(0))
		require.LessOrEqual(t, d.RetryAfter, 50*time.Millisecond+5*time.Millisecond, "one emission interval at 20/s")
		time.Sleep(d.RetryAfter + 5*time.Millisecond)
		d, err = s.Allow(ctx, "refill", 20, 5)
		require.NoError(t, err)
		require.True(t, d.Allowed, "after retry-after one request is admitted again")
	})

	t.Run("keys are independent", func(t *testing.T) {
		for range 3 {
			_, err := s.Allow(ctx, "a", 1, 3)
			require.NoError(t, err)
		}
		d, err := s.Allow(ctx, "b", 1, 3)
		require.NoError(t, err)
		require.True(t, d.Allowed)
	})
}

func TestBreaker_Integration(t *testing.T) {
	r := testenv.StartRedis(t)
	s, err := redisx.New(r.Client(t), redisx.Config{CallTimeout: 50 * time.Millisecond, FailureThreshold: 3, OpenFor: 500 * time.Millisecond},
		noop.NewMeterProvider().Meter("t"))
	require.NoError(t, err)
	ctx := t.Context()
	require.NoError(t, s.Set(ctx, "k", []byte("v"), time.Minute))

	r.Pause(t)
	t.Cleanup(func() { r.Unpause(t) })
	for range 3 {
		start := time.Now()
		_, _, err = s.Get(ctx, "k")
		require.ErrorIs(t, err, redisx.ErrUnavailable)
		require.Less(t, time.Since(start), 250*time.Millisecond, "each call is bounded by the call timeout")
	}
	start := time.Now()
	_, _, err = s.Get(ctx, "k")
	require.ErrorIs(t, err, redisx.ErrUnavailable)
	require.Less(t, time.Since(start), 5*time.Millisecond, "open breaker answers without touching Redis")

	r.Unpause(t)
	require.Eventually(t, func() bool {
		v, ok, err := s.Get(ctx, "k")
		return err == nil && ok && string(v) == "v"
	}, 5*time.Second, 50*time.Millisecond, "breaker closes after Redis recovers")
}
