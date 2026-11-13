// Package migrations embeds the control-plane goose migrations and applies them (ADR-0008, ADR-0018).
package migrations

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"log/slog"

	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"

	"github.com/zodixdev/infrapilot/pkg/postgres"
)

// FS holds the SQL migrations, applied in file-name order.
//
//go:embed *.sql
var FS embed.FS

// Migrate applies every pending migration as the migrator role (MigrationConfig: one connection, no statement
// timeout, 3 s lock timeout). goose serializes concurrent runners with a Postgres session advisory lock, so
// parallel Helm hook jobs are safe; a lock timeout fails the run and the Job's retry applies it again.
func Migrate(ctx context.Context, url string, logger *slog.Logger) error {
	db, err := postgres.OpenSQL(postgres.MigrationConfig(postgres.Config{URL: url, ApplicationName: "control-plane-migrate"}))
	if err != nil {
		return fmt.Errorf("migrations: migrate: %w", err)
	}
	defer func() { _ = db.Close() }()
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return fmt.Errorf("migrations: migrate: %w", err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, FS, goose.WithSessionLocker(locker))
	if err != nil {
		return fmt.Errorf("migrations: migrate: %w", err)
	}
	results, err := provider.Up(ctx)
	for _, r := range results {
		if r.Error == nil {
			logger.InfoContext(ctx, "migration applied", slog.Int64("version", r.Source.Version),
				slog.String("file", r.Source.Path), slog.Duration("duration", r.Duration))
		}
	}
	if err != nil {
		var partial *goose.PartialError
		if errors.As(err, &partial) && partial.Failed != nil {
			return fmt.Errorf("migrations: migration %d failed: %w", partial.Failed.Source.Version, partial.Err)
		}
		return fmt.Errorf("migrations: migrate: %w", err)
	}
	return nil
}
