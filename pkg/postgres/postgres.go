// Package postgres opens the pgx connection pool used by InfraPilot services and provides
// tenant-scoped transactions (ADR-0008, ADR-0010, ADR-0018).
//
// The underlying pgxpool is deliberately not exported: every statement runs inside WithTx (tenant
// context set) or WithSystemTx (tenant context explicitly cleared), so no caller can leave a
// session-level app.org_id on a pooled connection for the next user of that connection.
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

// Disabled turns a server-side timeout off (sent as 0). Use it for migration and batch pools only.
const Disabled time.Duration = -1

// Config configures the pool. URL carries the runtime (`<svc>_app`) credentials.
type Config struct {
	URL string
	// ApplicationName is visible in pg_stat_activity and labels the pool metrics (service or job name).
	ApplicationName string
	MaxConns        int32
	MinConns        int32
	MaxConnLifetime time.Duration
	MaxConnIdleTime time.Duration
	// HealthCheckPeriod is pgxpool's background check interval for idle connections.
	HealthCheckPeriod time.Duration
	ConnectTimeout    time.Duration
	// Client-side overrides of the server-side guards. The role defaults set at provisioning time
	// (ADR-0018) are the source of truth; these make the intent explicit and work without them.
	// Zero means "use the default", Disabled means "no limit".
	StatementTimeout                time.Duration
	LockTimeout                     time.Duration
	IdleInTransactionSessionTimeout time.Duration
	// RollbackTimeout bounds the rollback issued after an error or panic (default 5s).
	RollbackTimeout time.Duration
}

// Defaults fills zero values with production-safe defaults for request-serving pools.
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
	if c.StatementTimeout == 0 {
		c.StatementTimeout = 30 * time.Second
	}
	if c.LockTimeout == 0 {
		c.LockTimeout = 5 * time.Second
	}
	if c.IdleInTransactionSessionTimeout == 0 {
		c.IdleInTransactionSessionTimeout = 60 * time.Second
	}
	if c.RollbackTimeout <= 0 {
		c.RollbackTimeout = 5 * time.Second
	}
	return c
}

// MigrationConfig returns cfg tuned for schema migrations: no statement limit (backfills,
// CREATE INDEX CONCURRENTLY), a short lock timeout so DDL never queues behind traffic for long
// (callers retry on SQLSTATE 55P03), and a generous idle-in-transaction limit. One connection.
func MigrationConfig(cfg Config) Config {
	cfg.MaxConns = 1
	cfg.MinConns = 0
	cfg.StatementTimeout = Disabled
	cfg.LockTimeout = 3 * time.Second
	cfg.IdleInTransactionSessionTimeout = 5 * time.Minute
	return cfg
}

// ErrBypassRLS is returned by WithTx on a pool whose role bypasses row-level security: the tenant
// context would be silently ignored and the transaction would see every tenant.
var ErrBypassRLS = errors.New("postgres: WithTx on a BYPASSRLS role; use WithSystemTx and filter explicitly")

// Pool is a tenant-aware connection pool.
type Pool struct {
	pool      *pgxpool.Pool
	log       *slog.Logger
	cfg       Config
	bypassRLS bool
	metrics   metric.Registration
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

// Open parses cfg, applies defaults and server-side timeouts, connects, pings and detects whether the
// role bypasses RLS. Fails fast.
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
	p := &Pool{pool: pool, log: o.log, cfg: cfg}
	pingCtx, cancel := context.WithTimeout(ctx, cfg.ConnectTimeout)
	defer cancel()
	if err := pool.QueryRow(pingCtx, `SELECT rolbypassrls OR rolsuper FROM pg_roles WHERE rolname = current_user`).Scan(&p.bypassRLS); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: ping: %w", err)
	}
	if o.mp != nil {
		if err := p.registerMetrics(o.mp); err != nil {
			pool.Close()
			return nil, err
		}
	}
	o.log.InfoContext(ctx, "postgres pool ready",
		slog.String("host", pc.ConnConfig.Host), slog.String("database", pc.ConnConfig.Database),
		slog.String("user", pc.ConnConfig.User), slog.Int("max_conns", int(cfg.MaxConns)),
		slog.Bool("bypass_rls", p.bypassRLS))
	return p, nil
}

func millis(d time.Duration) string {
	if d <= 0 {
		return "0"
	}
	return strconv.FormatInt(d.Milliseconds(), 10)
}

// BypassesRLS reports whether the pool's role ignores row-level security (`_batch` roles).
func (p *Pool) BypassesRLS() bool { return p.bypassRLS }

// Stat exposes pool statistics.
func (p *Pool) Stat() *pgxpool.Stat { return p.pool.Stat() }

// HealthCheck returns a readiness probe: a round trip on a pooled connection.
func (p *Pool) HealthCheck() func(ctx context.Context) error {
	return func(ctx context.Context) error {
		var one int
		if err := p.pool.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
			return fmt.Errorf("postgres: %w", err)
		}
		return nil
	}
}

// Close releases all connections, waiting for acquired ones to be returned.
func (p *Pool) Close() {
	if p.metrics != nil {
		_ = p.metrics.Unregister() // only fails if already unregistered
		p.metrics = nil
	}
	p.pool.Close()
}

// Shutdown closes the pool within ctx; it satisfies app.Closer's signature. If ctx expires first the
// close keeps running in the background and finishes as soon as the last connection is released
// (which the preceding shutdown steps have already forced by stopping all servers).
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
