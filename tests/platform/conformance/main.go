// Command conformance is the reference workload for the infrapilot-lib Helm chart and the kind e2e
// check. It wires pkg/ exactly like a real service (config → log → telemetry → health → postgres →
// servers → app) and exposes a few endpoints that make shutdown, recovery and tracing observable.
//
//	conformance            serve (default)
//	conformance migrate    apply the probe schema as the migrator role (Helm pre-upgrade hook)
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zodixdev/infrapilot/pkg/app"
	"github.com/zodixdev/infrapilot/pkg/config"
	"github.com/zodixdev/infrapilot/pkg/grpcserver"
	"github.com/zodixdev/infrapilot/pkg/health"
	"github.com/zodixdev/infrapilot/pkg/httpserver"
	"github.com/zodixdev/infrapilot/pkg/log"
	"github.com/zodixdev/infrapilot/pkg/postgres"
	"github.com/zodixdev/infrapilot/pkg/telemetry"
)

// Config is the service configuration; DATABASE_URL is optional so the binary also runs without Postgres.
type Config struct {
	config.Common
	DatabaseURL string `env:"DATABASE_URL" envDefault:""`
	// MigrateTimeout bounds the whole migration including waiting for the database (Helm hook deadline is 600s).
	MigrateTimeout time.Duration `env:"MIGRATE_TIMEOUT" envDefault:"5m"`
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		// Redacting fallback: config and connection errors may contain DSNs.
		log.Fallback(os.Stderr).Error("conformance exited with error", slog.Any("error", err))
		os.Exit(1)
	}
}

func run(args []string) error {
	cfg, err := config.Load[Config]()
	if err != nil {
		return err
	}
	logCfg := log.Config{Level: cfg.Log.Level, Format: cfg.Log.Format, AddSource: cfg.Log.AddSource}
	logger, err := log.New(logCfg, os.Stdout, cfg.ServiceName, cfg.Version, string(cfg.Environment))
	if err != nil {
		return err
	}
	ctx := context.Background()
	if len(args) > 0 && args[0] == "migrate" {
		migrateCtx, cancel := context.WithTimeout(ctx, cfg.MigrateTimeout)
		defer cancel()
		return migrate(migrateCtx, cfg, logger)
	}
	return serve(ctx, cfg, logCfg, logger)
}

// migrate creates the probe table idempotently as the migrator role (ADR-0018). The database may still be
// starting (Helm does not wait for it), so the connection is retried with backoff until ctx expires.
func migrate(ctx context.Context, cfg Config, logger *slog.Logger) error {
	if cfg.DatabaseURL == "" {
		return errors.New("migrate: DATABASE_URL is required")
	}
	pool, err := openWithRetry(ctx, postgres.MigrationConfig(postgres.Config{URL: cfg.DatabaseURL, ApplicationName: cfg.ServiceName + "-migrate"}), logger)
	if err != nil {
		return err
	}
	defer pool.Close()
	return pool.WithSystemTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		stmts := []string{
			`CREATE TABLE IF NOT EXISTS conformance_probe (
			   id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			   release_version TEXT NOT NULL,
			   applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
			`GRANT SELECT, INSERT ON conformance_probe TO conformance_app`,
		}
		for _, s := range stmts {
			if _, err := tx.Exec(ctx, s); err != nil {
				return fmt.Errorf("migrate: %w", err)
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO conformance_probe (release_version) VALUES ($1)`, cfg.Version); err != nil {
			return fmt.Errorf("migrate: record release: %w", err)
		}
		logger.InfoContext(ctx, "migration applied", slog.String("release_version", cfg.Version))
		return nil
	})
}

func openWithRetry(ctx context.Context, pcfg postgres.Config, logger *slog.Logger) (*postgres.Pool, error) {
	backoff := 500 * time.Millisecond
	for attempt := 1; ; attempt++ {
		pool, err := postgres.Open(ctx, pcfg, postgres.WithLogger(logger))
		if err == nil {
			return pool, nil
		}
		logger.WarnContext(ctx, "database not reachable yet", slog.Int("attempt", attempt), slog.Any("error", err))
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("migrate: database unreachable: %w", errors.Join(err, ctx.Err()))
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 5*time.Second)
	}
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

func serve(ctx context.Context, cfg Config, logCfg log.Config, logger *slog.Logger) (err error) {
	// Until app.Run owns them, resources opened here are closed on any startup error.
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

	tel, err := telemetry.Setup(ctx, telemetry.Config{
		OTLPEndpoint: cfg.Telemetry.ExporterOTLPEndpoint, OTLPInsecure: cfg.Telemetry.ExporterOTLPInsecure,
		TraceSampleRatio: cfg.Telemetry.TracesSamplerArg, MetricsPush: cfg.Telemetry.MetricsPush, LogsPush: cfg.Telemetry.LogsPush,
		ExportInterval: cfg.Telemetry.ExportInterval, ExportTimeout: cfg.Telemetry.ExportTimeout,
	}, telemetry.Service{Name: cfg.ServiceName, Version: cfg.Version, Environment: string(cfg.Environment)}, logger)
	if err != nil {
		return err
	}
	cleanup = append(cleanup, tel.Shutdown)
	if h := tel.LogHandler(); h != nil {
		if logger, err = log.New(logCfg, os.Stdout, cfg.ServiceName, cfg.Version, string(cfg.Environment), h); err != nil {
			return err
		}
	}
	reg := health.NewRegistry(health.WithLogger(logger), health.WithMeter(tel.Meter("health")))

	var pool *postgres.Pool
	if cfg.DatabaseURL != "" {
		pool, err = postgres.Open(ctx, postgres.Config{URL: cfg.DatabaseURL, ApplicationName: cfg.ServiceName, MaxConns: 5},
			postgres.WithLogger(logger), postgres.WithTracerProvider(tel.TracerProvider()), postgres.WithMeterProvider(tel.MeterProvider()))
		if err != nil {
			return err
		}
		cleanup = append(cleanup, pool.Shutdown)
		reg.Register("postgres", pool.HealthCheck(), health.Periodic(5*time.Second))
	}

	grpcSrv, err := grpcserver.New(ctx, grpcserver.Config{
		Addr: cfg.GRPC.Addr, MaxRecvMsgBytes: cfg.GRPC.MaxRecvMsgBytes, ConnectionTimeout: cfg.GRPC.ConnectionTimeout,
		KeepaliveTime: cfg.GRPC.KeepaliveTime, KeepaliveTimeout: cfg.GRPC.KeepaliveTimeout, MaxConnectionAge: cfg.GRPC.MaxConnectionAge,
		MaxConnectionAgeGrace: cfg.GRPC.MaxConnectionAgeGrace, EnableReflection: cfg.GRPC.EnableReflection,
		MaxConcurrentStreams: cfg.GRPC.MaxConcurrentStreams,
		TLSCertFile:          cfg.GRPC.TLSCertFile, TLSKeyFile: cfg.GRPC.TLSKeyFile, TLSClientCAFile: cfg.GRPC.TLSClientCAFile,
	}, grpcserver.WithLogger(logger), grpcserver.WithTracerProvider(tel.TracerProvider()), grpcserver.WithMeterProvider(tel.MeterProvider()))
	if err != nil {
		return err
	}
	cleanup = append(cleanup, grpcSrv.Shutdown)

	mws, err := httpserver.Standard(httpserver.Options{Logger: logger, TracerProvider: tel.TracerProvider(),
		MeterProvider: tel.MeterProvider(), ServerName: cfg.ServiceName, MaxBodyBytes: cfg.HTTP.MaxBodyBytes})
	if err != nil {
		return err
	}
	httpCfg := httpserver.Config{
		Addr: cfg.HTTP.Addr, ReadHeaderTimeout: cfg.HTTP.ReadHeaderTimeout, ReadTimeout: cfg.HTTP.ReadTimeout,
		WriteTimeout: cfg.HTTP.WriteTimeout, IdleTimeout: cfg.HTTP.IdleTimeout, MaxHeaderBytes: cfg.HTTP.MaxHeaderBytes,
	}
	public, err := httpserver.New(ctx, httpCfg, httpserver.Chain(routes(cfg, pool), mws...), httpserver.WithName("http"), httpserver.WithLogger(logger))
	if err != nil {
		return err
	}
	cleanup = append(cleanup, public.Shutdown)
	adminCfg := httpCfg
	adminCfg.Addr, adminCfg.WriteTimeout = cfg.Admin.Addr, cfg.Admin.WriteTimeout
	admin, err := httpserver.New(ctx, adminCfg,
		httpserver.AdminHandler(reg.LivenessHandler(), reg.ReadinessHandler(), tel.MetricsHandler(), cfg.Admin.Pprof),
		httpserver.WithName("admin"), httpserver.WithLogger(logger))
	if err != nil {
		return err
	}
	cleanup = append(cleanup, admin.Shutdown)

	a, err := app.New(app.Config{ShutdownDelay: cfg.Shutdown.Delay, ShutdownTimeout: cfg.Shutdown.Timeout},
		app.WithLogger(logger), app.WithReadinessGate(gates{reg: reg, grpc: grpcSrv}))
	if err != nil {
		return err
	}
	// Start order = dependency order; shutdown runs in reverse: public → admin → grpc → health → pool → telemetry.
	a.Add(app.Closer("telemetry", tel.Shutdown))
	if pool != nil {
		a.Add(app.Closer("postgres", pool.Shutdown))
	}
	a.Add(app.Func("health", reg.Run, nil), grpcSrv, admin, public)
	cleanup = nil // app.Run owns every resource from here on
	return a.Run(ctx)
}

func routes(cfg Config, pool *postgres.Pool) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/probe", func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{"service": cfg.ServiceName, "version": cfg.Version, "request_id": httpserver.RequestIDFromContext(r.Context())}
		if pool != nil {
			var rows int64
			err := pool.WithSystemTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT count(*) FROM conformance_probe`).Scan(&rows)
			}, postgres.ReadOnly())
			if err != nil {
				httpserver.WriteProblem(w, r, http.StatusServiceUnavailable, "unavailable", "database unavailable")
				return
			}
			resp["probe_rows"] = rows
		}
		writeJSON(w, resp)
	})
	// /v1/slow?ms=N keeps a request in flight so graceful shutdown can be observed (max 10 s).
	mux.HandleFunc("GET /v1/slow", func(w http.ResponseWriter, r *http.Request) {
		ms, err := strconv.Atoi(r.URL.Query().Get("ms"))
		if err != nil || ms < 0 || ms > 10_000 {
			httpserver.WriteProblem(w, r, http.StatusBadRequest, "validation_failed", "ms must be an integer in [0, 10000]")
			return
		}
		select {
		case <-time.After(time.Duration(ms) * time.Millisecond):
			writeJSON(w, map[string]any{"slept_ms": ms})
		case <-r.Context().Done():
		}
	})
	// /v1/panic proves the recovery middleware returns problem+json and counts the request as a 500.
	mux.HandleFunc("GET /v1/panic", func(http.ResponseWriter, *http.Request) { panic("conformance: deliberate panic") })
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
