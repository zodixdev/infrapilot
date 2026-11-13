// Package redisx is the gateway's Redis access (ADR-0026): every call has a short deadline and goes through a
// circuit breaker, so a Redis outage costs at most a few slow requests before callers degrade (rate limiting
// fails open, caches fall back to gRPC — F03). Redis holds no source-of-truth data (data.md §1).
package redisx

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// ErrUnavailable is returned while the breaker is open or when a call fails or times out.
var ErrUnavailable = errors.New("redisx: redis unavailable")

// Config tunes deadlines and the breaker.
type Config struct {
	// CallTimeout bounds every Redis round trip (default 50 ms).
	CallTimeout time.Duration
	// FailureThreshold consecutive failures open the breaker (default 5).
	FailureThreshold int
	// OpenFor is how long the breaker stays open before one probe call is let through (default 5 s).
	OpenFor time.Duration
	// KeyPrefix namespaces every key (default "gw:").
	KeyPrefix string
}

// Store wraps a Redis client with deadlines, a breaker and metrics.
type Store struct {
	rdb    redis.UniversalClient
	cfg    Config
	now    func() time.Time
	errors metric.Int64Counter

	mu        sync.Mutex
	failures  int
	openUntil time.Time
	probing   bool
}

// New returns a Store; it does not connect eagerly (startup must not depend on Redis, F03). The client must be
// created with ContextTimeoutEnabled, otherwise go-redis ignores the per-call deadline and waits for its socket
// timeouts (see NewClient).
func New(rdb redis.UniversalClient, cfg Config, meter metric.Meter) (*Store, error) {
	if cfg.CallTimeout <= 0 {
		cfg.CallTimeout = 50 * time.Millisecond
	}
	if cfg.FailureThreshold <= 0 {
		cfg.FailureThreshold = 5
	}
	if cfg.OpenFor <= 0 {
		cfg.OpenFor = 5 * time.Second
	}
	if cfg.KeyPrefix == "" {
		cfg.KeyPrefix = "gw:"
	}
	s := &Store{rdb: rdb, cfg: cfg, now: time.Now}
	var err error
	if s.errors, err = meter.Int64Counter("gateway_redis_errors_total", metric.WithDescription("Failed or skipped Redis calls, by reason")); err != nil {
		return nil, fmt.Errorf("redisx: %w", err)
	}
	_, err = meter.Int64ObservableGauge("gateway_redis_breaker_open", metric.WithDescription("1 while the Redis circuit breaker is open"),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			s.mu.Lock()
			open := s.now().Before(s.openUntil)
			s.mu.Unlock()
			if open {
				o.Observe(1)
			} else {
				o.Observe(0)
			}
			return nil
		}))
	if err != nil {
		return nil, fmt.Errorf("redisx: %w", err)
	}
	return s, nil
}

// do runs fn with the call deadline when the breaker allows it and records the outcome.
func (s *Store) do(ctx context.Context, fn func(ctx context.Context) error) error {
	s.mu.Lock()
	if s.now().Before(s.openUntil) {
		s.mu.Unlock()
		s.errors.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "breaker_open")))
		return ErrUnavailable
	}
	half := !s.openUntil.IsZero() // the breaker just closed after being open: let a single probe through
	if half && s.probing {
		s.mu.Unlock()
		s.errors.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "breaker_probing")))
		return ErrUnavailable
	}
	s.probing = half
	s.mu.Unlock()

	cctx, cancel := context.WithTimeout(ctx, s.cfg.CallTimeout)
	err := fn(cctx)
	cancel()
	if errors.Is(err, redis.Nil) {
		err = nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.probing = false
	if err == nil {
		s.failures, s.openUntil = 0, time.Time{}
		return nil
	}
	if ctx.Err() != nil {
		return fmt.Errorf("redisx: %w", ctx.Err()) // the caller gave up; not Redis' fault
	}
	s.failures++
	if s.failures >= s.cfg.FailureThreshold || half {
		s.openUntil = s.now().Add(s.cfg.OpenFor)
	}
	s.errors.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "error")))
	return fmt.Errorf("%w: %w", ErrUnavailable, err)
}

// Get returns the value and whether it exists.
func (s *Store) Get(ctx context.Context, key string) ([]byte, bool, error) {
	var val []byte
	found := false
	err := s.do(ctx, func(ctx context.Context) error {
		b, err := s.rdb.Get(ctx, s.cfg.KeyPrefix+key).Bytes()
		if err == nil {
			val, found = b, true
		}
		return err
	})
	return val, found, err
}

// Set stores a value with a TTL.
func (s *Store) Set(ctx context.Context, key string, val []byte, ttl time.Duration) error {
	return s.do(ctx, func(ctx context.Context) error {
		return s.rdb.Set(ctx, s.cfg.KeyPrefix+key, val, ttl).Err()
	})
}

// Ping checks connectivity (readiness reporting only; the gateway stays ready without Redis).
func (s *Store) Ping(ctx context.Context) error {
	return s.do(ctx, func(ctx context.Context) error { return s.rdb.Ping(ctx).Err() })
}

// NewClient returns a go-redis client configured for the Store: per-call context deadlines are honored, no
// client-side retries (the breaker decides), small pool timeouts.
func NewClient(opts *redis.Options) *redis.Client {
	o := *opts
	o.ContextTimeoutEnabled = true
	o.MaxRetries = -1
	if o.DialTimeout == 0 {
		o.DialTimeout = 500 * time.Millisecond
	}
	if o.PoolTimeout == 0 {
		o.PoolTimeout = 100 * time.Millisecond
	}
	return redis.NewClient(&o)
}

// gcra is the generic cell rate algorithm in one atomic script: the key holds the theoretical arrival time
// (TAT). Times are seconds since 2017-01-01 so float precision stays sub-microsecond.
var gcra = redis.NewScript(`
local key = KEYS[1]
local burst = tonumber(ARGV[1])
local interval = tonumber(ARGV[2])
local t = redis.call("TIME")
local now = (tonumber(t[1]) - 1483228800) + tonumber(t[2]) / 1000000
local tat = tonumber(redis.call("GET", key) or now)
if tat < now then tat = now end
local new_tat = tat + interval
local allow_at = new_tat - burst * interval
local diff = now - allow_at
if diff < 0 then
  return {0, 0, tostring(-diff), tostring(tat - now)}
end
local ttl = math.ceil(new_tat - now)
if ttl < 1 then ttl = 1 end
redis.call("SET", key, tostring(new_tat), "EX", ttl)
return {1, math.floor(diff / interval + 1e-4), "0", tostring(new_tat - now)}
`)

// Decision is a rate-limit verdict.
type Decision struct {
	Allowed    bool
	Remaining  int
	RetryAfter time.Duration // when denied: wait at least this long
	ResetAfter time.Duration // until the bucket is full again
}

// Allow consumes one request from the bucket key with the given steady rate (per second) and burst.
func (s *Store) Allow(ctx context.Context, key string, ratePerSecond float64, burst int) (Decision, error) {
	if ratePerSecond <= 0 || burst <= 0 {
		return Decision{}, errors.New("redisx: rate and burst must be positive")
	}
	var d Decision
	err := s.do(ctx, func(ctx context.Context) error {
		res, err := gcra.Run(ctx, s.rdb, []string{s.cfg.KeyPrefix + "rl:" + key}, burst, strconv.FormatFloat(1/ratePerSecond, 'f', -1, 64)).Slice()
		if err != nil {
			return err
		}
		if len(res) != 4 {
			return fmt.Errorf("gcra: unexpected reply %v", res)
		}
		allowed, _ := res[0].(int64)
		remaining, _ := res[1].(int64)
		d = Decision{Allowed: allowed == 1, Remaining: int(remaining), RetryAfter: seconds(res[2]), ResetAfter: seconds(res[3])}
		return nil
	})
	return d, err
}

func seconds(v any) time.Duration {
	s, _ := v.(string)
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 {
		return 0
	}
	return time.Duration(f * float64(time.Second))
}
