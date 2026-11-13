// Command control-plane serves organizations, projects, memberships, resources, API keys and authorization
// decisions over internal gRPC (ADR-0002, ADR-0022).
//
//	control-plane            serve (default)
//	control-plane migrate    apply schema migrations as <svc>_migrator (Helm pre-install/pre-upgrade hook)
//	control-plane purge      apply retention as <svc>_batch (CronJob, ADR-0027)
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/zodixdev/infrapilot/pkg/config"
	"github.com/zodixdev/infrapilot/pkg/log"
	"github.com/zodixdev/infrapilot/services/control-plane/internal/app"
)

// Config is the control-plane configuration.
type Config struct {
	config.Common
	// DatabaseURL connects as controlplane_app (RLS enforced). Required by serve.
	DatabaseURL string `env:"DATABASE_URL" envDefault:""`
	// MigrationDatabaseURL connects as controlplane_migrator. Required by migrate.
	MigrationDatabaseURL string `env:"MIGRATION_DATABASE_URL" envDefault:""`
	// BatchDatabaseURL connects as controlplane_batch. Required by purge.
	BatchDatabaseURL string `env:"BATCH_DATABASE_URL" envDefault:""`
	DBMaxConns       int32  `env:"DB_MAX_CONNS" envDefault:"10"`
	// EdgeIdentities are the workload identities allowed to act for end users (ADR-0025).
	EdgeIdentities []string      `env:"EDGE_IDENTITIES" envSeparator:"," envDefault:""`
	MigrateTimeout time.Duration `env:"MIGRATE_TIMEOUT" envDefault:"9m"`
	PurgeTimeout   time.Duration `env:"PURGE_TIMEOUT" envDefault:"50m"`
}

// Validate checks settings shared by every mode; mode-specific requirements are checked by the mode.
func (c *Config) Validate() error {
	errs := []error{c.Common.Validate()}
	if c.DBMaxConns < 1 || c.DBMaxConns > 100 {
		errs = append(errs, errors.New("DB_MAX_CONNS must be between 1 and 100"))
	}
	for _, id := range c.EdgeIdentities {
		if !strings.HasPrefix(id, "spiffe://") {
			errs = append(errs, fmt.Errorf("identity %q must be a spiffe:// URI", id))
		}
	}
	if c.MigrateTimeout <= 0 || c.PurgeTimeout <= 0 {
		errs = append(errs, errors.New("MIGRATE_TIMEOUT and PURGE_TIMEOUT must be positive"))
	}
	return errors.Join(errs...)
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		// Redacting fallback: config and connection errors may contain DSNs.
		log.Fallback(os.Stderr).Error("control-plane exited with error", slog.Any("error", err))
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
	opts := app.Options{
		Common: cfg.Common, LogConfig: logCfg, Logger: logger,
		DatabaseURL: cfg.DatabaseURL, MigrationDatabaseURL: cfg.MigrationDatabaseURL, BatchDatabaseURL: cfg.BatchDatabaseURL,
		DBMaxConns: cfg.DBMaxConns, EdgeIdentities: cfg.EdgeIdentities,
	}
	ctx := context.Background()
	mode := "serve"
	if len(args) > 0 {
		mode = args[0]
	}
	switch mode {
	case "serve":
		return app.Serve(ctx, opts)
	case "migrate":
		mctx, cancel := context.WithTimeout(ctx, cfg.MigrateTimeout)
		defer cancel()
		return app.Migrate(mctx, opts)
	case "purge":
		pctx, cancel := context.WithTimeout(ctx, cfg.PurgeTimeout)
		defer cancel()
		return app.Purge(pctx, opts)
	default:
		return fmt.Errorf("unknown command %q (want serve, migrate or purge)", mode)
	}
}
