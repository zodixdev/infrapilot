// Package postgres opens the pgx connection pool used by InfraPilot services and provides
// tenant-scoped transactions (ADR-0008, ADR-0010, ADR-0018).
package postgres

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Config configures the pool. URL carries the runtime (`<svc>_app`) credentials.
type Config struct {
	URL string
	// ApplicationName is visible in pg_stat_activity (service name, or job name for batch roles).
	ApplicationName string
	MaxConns        int32
	MinConns        int32
	MaxConnLifetime time.Duration
	MaxConnIdleTime time.Duration
	// HealthCheckPeriod is pgxpool's background check interval for idle connections.
	HealthCheckPeriod time.Duration
	ConnectTimeout    time.Duration
	// Server-side guards applied to every connection.
	StatementTimeout                time.Duration
	LockTimeout                     time.Duration
	IdleInTransactionSessionTimeout time.Duration
}

// Defaults fills zero values with production-safe defaults.
func (c Config) Defaults() Config {
	if c.MaxConns <= 0 {
		c.MaxConns = 10
	}
	if c.MinConns < 0 {
		c.MinConns = 0
	}
	if c.MaxConnLifetime <= 0 {
		c.MaxConnLifetime = 30 * time.Minute
	}
	if c.MaxConnIdleTime <= 0 {
		c.MaxConnIdleTime = 5 * time.Minute
	}
	if c.HealthCheckPeriod <= 0 {
		c.HealthCheckPeriod = 30 * time.Second
	}
	if c.ConnectTimeout <= 0 {
		c.ConnectTimeout = 5 * time.Second
	}
	if c.StatementTimeout <= 0 {
		c.StatementTimeout = 30 * time.Second
	}
	if c.LockTimeout <= 0 {
		c.LockTimeout = 5 * time.Second
	}
	if c.IdleInTransactionSessionTimeout <= 0 {
		c.IdleInTransactionSessionTimeout = 60 * time.Second
	}
	return c
}

// Pool wraps pgxpool.Pool with InfraPilot conventions.
type Pool struct {
	*pgxpool.Pool
	log *slog.Logger
}

// Option configures Open.
type Option func(*openOptions)

type openOptions struct {
	log *slog.Logger
	tp  trace.TracerProvider
	mp  metric.MeterProvider
}

// WithLogger sets the logger.
func WithLogger(l *slog.Logger) Option { return func(o *openOptions) { o.log = l } }

// WithTracerProvider enables query spans via otelpgx (statement text trimmed to the first keyword).
func WithTracerProvider(tp trace.TracerProvider) Option { return func(o *openOptions) { o.tp = tp } }

// WithMeterProvider exports pool statistics (ADR-0008: pool sizes are explicit and exposed as metrics).
func WithMeterProvider(mp metric.MeterProvider) Option { return func(o *openOptions) { o.mp = mp } }

// Open parses cfg, applies defaults and server-side timeouts, connects and pings. Fails fast.
func Open(ctx context.Context, cfg Config, opts ...Option) (*Pool, error) {
	if cfg.URL == "" {
		return nil, errors.New("postgres: URL is required")
	}
	cfg = cfg.Defaults()
	if cfg.MinConns > cfg.MaxConns {
		return nil, fmt.Errorf("postgres: MinConns (%d) > MaxConns (%d)", cfg.MinConns, cfg.MaxConns)
	}
	o := openOptions{log: slog.New(slog.DiscardHandler)}
	for _, opt := range opts {
		opt(&o)
	}
	pc, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse url: %w", err) // pgx redacts the password in this error
	}
	pc.MaxConns = cfg.MaxConns
	pc.MinConns = cfg.MinConns
	pc.MaxConnLifetime = cfg.MaxConnLifetime
	pc.MaxConnIdleTime = cfg.MaxConnIdleTime
	pc.HealthCheckPeriod = cfg.HealthCheckPeriod
	pc.ConnConfig.ConnectTimeout = cfg.ConnectTimeout
	if pc.ConnConfig.RuntimeParams == nil {
		pc.ConnConfig.RuntimeParams = map[string]string{}
	}
	if cfg.ApplicationName != "" {
		pc.ConnConfig.RuntimeParams["application_name"] = cfg.ApplicationName
	}
	pc.ConnConfig.RuntimeParams["statement_timeout"] = millis(cfg.StatementTimeout)
	pc.ConnConfig.RuntimeParams["lock_timeout"] = millis(cfg.LockTimeout)
	pc.ConnConfig.RuntimeParams["idle_in_transaction_session_timeout"] = millis(cfg.IdleInTransactionSessionTimeout)
	if o.tp != nil {
		pc.ConnConfig.Tracer = otelpgx.NewTracer(otelpgx.WithTracerProvider(o.tp))
	}

	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, fmt.Errorf("postgres: create pool: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, cfg.ConnectTimeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: ping: %w", err)
	}
	p := &Pool{Pool: pool, log: o.log}
	if o.mp != nil {
		if err := p.registerMetrics(o.mp); err != nil {
			pool.Close()
			return nil, err
		}
	}
	o.log.InfoContext(ctx, "postgres pool ready",
		slog.String("host", pc.ConnConfig.Host), slog.String("database", pc.ConnConfig.Database),
		slog.String("user", pc.ConnConfig.User), slog.Int("max_conns", int(cfg.MaxConns)))
	return p, nil
}

func millis(d time.Duration) string { return strconv.FormatInt(d.Milliseconds(), 10) }

// HealthCheck returns a readiness probe: a round trip on a pooled connection.
func (p *Pool) HealthCheck() func(ctx context.Context) error {
	return func(ctx context.Context) error {
		var one int
		if err := p.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
			return fmt.Errorf("postgres: %w", err)
		}
		return nil
	}
}

// Shutdown closes the pool; it satisfies app.Closer's signature. Close waits for acquired
// connections to be released, so it is bounded by ctx here.
func (p *Pool) Shutdown(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		p.Close()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("postgres: close: %w", ctx.Err())
	}
}
