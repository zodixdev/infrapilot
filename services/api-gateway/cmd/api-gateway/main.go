// Command api-gateway serves the public REST API (ADR-0003): it authenticates users (Keycloak JWT) and API keys,
// rate-limits, validates requests against the OpenAPI contract, pre-authorizes org-scoped routes and forwards to
// internal services over mTLS gRPC. It holds no business logic (ADR-0002).
package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/zodixdev/infrapilot/pkg/config"
	"github.com/zodixdev/infrapilot/pkg/log"
	"github.com/zodixdev/infrapilot/services/api-gateway/internal/app"
)

func main() {
	if err := run(); err != nil {
		// Redacting fallback: config errors may contain URLs or credentials.
		log.Fallback(os.Stderr).Error("api-gateway exited with error", slog.Any("error", err))
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load[app.Config]()
	if err != nil {
		return err
	}
	logCfg := log.Config{Level: cfg.Log.Level, Format: cfg.Log.Format, AddSource: cfg.Log.AddSource}
	logger, err := log.New(logCfg, os.Stdout, cfg.ServiceName, cfg.Version, string(cfg.Environment))
	if err != nil {
		return err
	}
	return app.Serve(context.Background(), &cfg, logCfg, logger)
}
