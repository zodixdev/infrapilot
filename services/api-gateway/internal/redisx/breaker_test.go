package redisx

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/metric/noop"
)

func newTestStore(t *testing.T, now *time.Time) *Store {
	t.Helper()
	s, err := New(nil, Config{FailureThreshold: 3, OpenFor: 5 * time.Second}, noop.NewMeterProvider().Meter("t"))
	require.NoError(t, err)
	s.now = func() time.Time { return *now }
	return s
}

func TestBreaker_StateMachine(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0)
	s := newTestStore(t, &now)
	boom := errors.New("connection refused")
	fail := func(context.Context) error { return boom }
	ok := func(context.Context) error { return nil }
	ctx := t.Context()

	for range 2 {
		require.ErrorIs(t, s.do(ctx, fail), ErrUnavailable)
	}
	require.NoError(t, s.do(ctx, ok), "below the threshold the breaker stays closed and a success resets it")
	for range 3 {
		require.ErrorIs(t, s.do(ctx, fail), ErrUnavailable)
	}
	calls := 0
	require.ErrorIs(t, s.do(ctx, func(context.Context) error { calls++; return nil }), ErrUnavailable)
	require.Zero(t, calls, "open breaker never touches Redis")

	now = now.Add(5 * time.Second)
	require.ErrorIs(t, s.do(ctx, fail), ErrUnavailable, "a failed probe re-opens immediately")
	require.ErrorIs(t, s.do(ctx, ok), ErrUnavailable)
	now = now.Add(5 * time.Second)
	require.NoError(t, s.do(ctx, ok), "a successful probe closes the breaker")
	require.NoError(t, s.do(ctx, ok))
}

func TestBreaker_LateFailuresDoNotExtendOpenPeriod(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0)
	s := newTestStore(t, &now)
	ctx := t.Context()
	release, started := make(chan struct{}), make(chan struct{})
	done := make(chan error)
	// A slow call admitted while closed...
	go func() {
		done <- s.do(ctx, func(context.Context) error { close(started); <-release; return errors.New("late") })
	}()
	<-started
	// ...then the breaker opens because of other failures.
	for range 3 {
		_ = s.do(ctx, func(context.Context) error { return errors.New("fail") })
	}
	s.mu.Lock()
	openUntil := s.openUntil
	s.mu.Unlock()
	now = now.Add(4 * time.Second)
	close(release)
	require.ErrorIs(t, <-done, ErrUnavailable)
	s.mu.Lock()
	defer s.mu.Unlock()
	require.Equal(t, openUntil, s.openUntil, "the late failure belongs to the previous state and is ignored")
}

func TestBreaker_CallerCancellationIsNotAFailure(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0)
	s := newTestStore(t, &now)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for range 5 {
		err := s.do(ctx, func(ctx context.Context) error { return ctx.Err() })
		require.ErrorIs(t, err, context.Canceled)
	}
	require.NoError(t, s.do(t.Context(), func(context.Context) error { return nil }), "breaker still closed")
}
