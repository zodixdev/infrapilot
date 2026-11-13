// Package app wires the control-plane process: telemetry, health, the Postgres pool, the gRPC server with its
// peer allow-list and principal interceptor, and the admin endpoints, under pkg/app's ordered lifecycle.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/zodixdev/infrapilot/pkg/app"
	"github.com/zodixdev/infrapilot/pkg/config"
	"github.com/zodixdev/infrapilot/pkg/grpcserver"
	"github.com/zodixdev/infrapilot/pkg/health"
	"github.com/zodixdev/infrapilot/pkg/httpserver"
	"github.com/zodixdev/infrapilot/pkg/log"
	"github.com/zodixdev/infrapilot/pkg/postgres"
	"github.com/zodixdev/infrapilot/pkg/telemetry"
	"github.com/zodixdev/infrapilot/services/control-plane/internal/service"
	"github.com/zodixdev/infrapilot/services/control-plane/internal/store"
	cpgrpc "github.com/zodixdev/infrapilot/services/control-plane/internal/transport/grpc"
	"github.com/zodixdev/infrapilot/services/control-plane/migrations"
)

// Options is the resolved configuration of one process.
type Options struct {
	Common                                              config.Common
	LogConfig                                           log.Config
	Logger                                              *slog.Logger
	DatabaseURL, MigrationDatabaseURL, BatchDatabaseURL string
	DBMaxConns                                          int32
	EdgeIdentities                                      []string
}

func setupTelemetry(ctx context.Context, o *Options) (*telemetry.Provider, error) {
	c := o.Common
	tel, err := telemetry.Setup(ctx, telemetry.Config{
		OTLPEndpoint: c.Telemetry.ExporterOTLPEndpoint, OTLPInsecure: c.Telemetry.ExporterOTLPInsecure,
		TraceSampleRatio: c.Telemetry.TracesSamplerArg, MetricsPush: c.Telemetry.MetricsPush, LogsPush: c.Telemetry.LogsPush,
		ExportInterval: c.Telemetry.ExportInterval, ExportTimeout: c.Telemetry.ExportTimeout,
	}, telemetry.Service{Name: c.ServiceName, Version: c.Version, Environment: string(c.Environment)}, o.Logger)
	if err != nil {
		return nil, err
	}
	if h := tel.LogHandler(); h != nil {
		if o.Logger, err = log.New(o.LogConfig, os.Stdout, c.ServiceName, c.Version, string(c.Environment), h); err != nil {
			return nil, errors.Join(err, tel.Shutdown(ctx))
		}
	}
	return tel, nil
}

// gates fans the readiness gate out to the health registry and the gRPC health service.
type gates struct {
	reg  *health.Registry
	grpc *grpcserver.Server
}

func (g gates) SetReady(ready bool) {
	g.reg.SetReady(ready)
	g.grpc.SetServing(ready)
}

// Serve runs the gRPC API until SIGTERM/SIGINT, then drains (AGENTS.md §5).
func Serve(ctx context.Context, o Options) (err error) {
	if o.DatabaseURL == "" {
		return errors.New("serve: DATABASE_URL is required")
	}
	if len(o.EdgeIdentities) == 0 {
		return errors.New("serve: EDGE_IDENTITIES is required (ADR-0025)")
	}
	c := o.Common
	var cleanup []func(context.Context) error
	defer func() {
		if err == nil {
			return
		}
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		for i := len(cleanup) - 1; i >= 0; i-- {
			_ = cleanup[i](closeCtx) // best effort: the startup error is what matters
		}
	}()

	tel, err := setupTelemetry(ctx, &o)
	if err != nil {
		return err
	}
	cleanup = append(cleanup, tel.Shutdown)
	logger := o.Logger
	reg := health.NewRegistry(health.WithLogger(logger), health.WithMeter(tel.Meter("health")))

	pool, err := postgres.Open(ctx, postgres.Config{URL: o.DatabaseURL, ApplicationName: c.ServiceName, MaxConns: o.DBMaxConns},
		postgres.WithLogger(logger), postgres.WithTracerProvider(tel.TracerProvider()), postgres.WithMeterProvider(tel.MeterProvider()))
	if err != nil {
		return err
	}
	cleanup = append(cleanup, pool.Shutdown)
	reg.Register("postgres", pool.HealthCheck(), health.Periodic(5*time.Second))

	svc, err := service.New(pool, service.WithLogger(logger))
	if err != nil {
		return err
	}
	insecureIdentity := c.GRPC.InsecurePeerIdentity
	grpcSrv, err := grpcserver.New(ctx, grpcserver.Config{
		Addr: c.GRPC.Addr, MaxRecvMsgBytes: c.GRPC.MaxRecvMsgBytes, ConnectionTimeout: c.GRPC.ConnectionTimeout,
		KeepaliveTime: c.GRPC.KeepaliveTime, KeepaliveTimeout: c.GRPC.KeepaliveTimeout, MaxConnectionAge: c.GRPC.MaxConnectionAge,
		MaxConnectionAgeGrace: c.GRPC.MaxConnectionAgeGrace, EnableReflection: c.GRPC.EnableReflection,
		MaxConcurrentStreams: c.GRPC.MaxConcurrentStreams,
		TLSCertFile:          c.GRPC.TLSCertFile, TLSKeyFile: c.GRPC.TLSKeyFile, TLSClientCAFile: c.GRPC.TLSClientCAFile,
	}, grpcserver.WithLogger(logger), grpcserver.WithTracerProvider(tel.TracerProvider()), grpcserver.WithMeterProvider(tel.MeterProvider()),
		grpcserver.WithPeerPolicy(grpcserver.PeerPolicy{Rules: cpgrpc.PeerRules(o.EdgeIdentities), InsecureIdentity: insecureIdentity}),
		grpcserver.WithUnaryInterceptors(cpgrpc.PrincipalInterceptor(svc, o.EdgeIdentities, logger)))
	if err != nil {
		return err
	}
	cleanup = append(cleanup, grpcSrv.Shutdown)
	cpgrpc.NewServer(svc, logger).Register(grpcSrv.Registrar())

	adminCfg := httpserver.Config{
		Addr: c.Admin.Addr, ReadHeaderTimeout: c.HTTP.ReadHeaderTimeout, ReadTimeout: c.HTTP.ReadTimeout,
		WriteTimeout: c.Admin.WriteTimeout, IdleTimeout: c.HTTP.IdleTimeout, MaxHeaderBytes: c.HTTP.MaxHeaderBytes,
	}
	admin, err := httpserver.New(ctx, adminCfg,
		httpserver.AdminHandler(reg.LivenessHandler(), reg.ReadinessHandler(), tel.MetricsHandler(), c.Admin.Pprof),
		httpserver.WithName("admin"), httpserver.WithLogger(logger))
	if err != nil {
		return err
	}
	cleanup = append(cleanup, admin.Shutdown)

	a, err := app.New(app.Config{ShutdownDelay: c.Shutdown.Delay, ShutdownTimeout: c.Shutdown.Timeout},
		app.WithLogger(logger), app.WithReadinessGate(gates{reg: reg, grpc: grpcSrv}))
	if err != nil {
		return err
	}
	// Start order = dependency order; shutdown runs in reverse: grpc → admin → health → pool → telemetry.
	a.Add(app.Closer("telemetry", tel.Shutdown), app.Closer("postgres", pool.Shutdown), app.Func("health", reg.Run, nil), admin, grpcSrv)
	cleanup = nil // app.Run owns every resource from here on
	return a.Run(ctx)
}

// Migrate applies the schema as the migrator role. The database may still be starting (Helm does not wait for
// it), so reachability is retried with backoff until ctx expires.
func Migrate(ctx context.Context, o Options) error {
	if o.MigrationDatabaseURL == "" {
		return errors.New("migrate: MIGRATION_DATABASE_URL is required")
	}
	if err := waitForDatabase(ctx, o.MigrationDatabaseURL, o.Logger); err != nil {
		return err
	}
	return migrations.Migrate(ctx, o.MigrationDatabaseURL, o.Logger)
}

func waitForDatabase(ctx context.Context, url string, logger *slog.Logger) error {
	backoff := 500 * time.Millisecond
	for attempt := 1; ; attempt++ {
		db, err := postgres.OpenSQL(postgres.Config{URL: url, ApplicationName: "control-plane-migrate", MaxConns: 1})
		if err == nil {
			err = db.PingContext(ctx)
			_ = db.Close()
		}
		if err == nil {
			return nil
		}
		logger.WarnContext(ctx, "database not reachable yet", slog.Int("attempt", attempt), slog.Any("error", err))
		select {
		case <-ctx.Done():
			return fmt.Errorf("database unreachable: %w", errors.Join(err, ctx.Err()))
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 5*time.Second)
	}
}

// Purge applies retention as the batch role and exports per-table counts and the last success time. As a
// short-lived job it pushes metrics over OTLP (TELEMETRY_METRICS_PUSH, ADR-0019); shutdown flushes them.
func Purge(ctx context.Context, o Options) (err error) {
	if o.BatchDatabaseURL == "" {
		return errors.New("purge: BATCH_DATABASE_URL is required")
	}
	tel, err := setupTelemetry(ctx, &o)
	if err != nil {
		return err
	}
	defer func() {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		err = errors.Join(err, tel.Shutdown(sctx))
	}()
	m := tel.Meter("github.com/zodixdev/infrapilot/services/control-plane/purge")
	rows, err := m.Int64Counter("controlplane_purge_rows_total", metric.WithDescription("Rows deleted by retention, by table"))
	if err != nil {
		return fmt.Errorf("purge: %w", err)
	}
	lastSuccess, err := m.Float64Gauge("controlplane_purge_last_success_timestamp_seconds",
		metric.WithDescription("Unix time of the last successful retention run"), metric.WithUnit("s"))
	if err != nil {
		return fmt.Errorf("purge: %w", err)
	}
	// application_name identifies the job in pg_stat_activity and statement logs (ADR-0018).
	pool, err := postgres.Open(ctx, postgres.Config{URL: o.BatchDatabaseURL, ApplicationName: "controlplane-purge", MaxConns: 1},
		postgres.WithLogger(o.Logger), postgres.WithTracerProvider(tel.TracerProvider()))
	if err != nil {
		return err
	}
	defer pool.Close()
	start := time.Now()
	deleted, err := store.Purge(ctx, pool, func(table string, n int64) {
		rows.Add(ctx, n, metric.WithAttributes(attribute.String("table", table)))
	})
	attrs := make([]any, 0, len(deleted)+2)
	for table, n := range deleted {
		attrs = append(attrs, slog.Int64(table, n))
	}
	attrs = append(attrs, slog.Duration("duration", time.Since(start)))
	if err != nil {
		o.Logger.ErrorContext(ctx, "retention purge failed", append(attrs, slog.Any("error", err))...)
		return err
	}
	lastSuccess.Record(ctx, float64(time.Now().Unix()))
	o.Logger.InfoContext(ctx, "retention purge completed", attrs...)
	return nil
}
