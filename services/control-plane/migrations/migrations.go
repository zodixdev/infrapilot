// Package migrations embeds the control-plane goose migrations and applies them (ADR-0008, ADR-0018).
package migrations

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"

	"github.com/zodixdev/infrapilot/pkg/postgres"
)

// FS holds the SQL migrations, applied in file-name order.
//
//go:embed *.sql
var FS embed.FS

// lockRetries bounds how often a migration that hit lock_timeout (SQLSTATE 55P03) is retried (ADR-0018): the
// short lock timeout keeps DDL from queueing behind traffic, and the retry gives it another chance.
const lockRetries = 5

// Migrate applies pending migrations and backfills resource state as the migrator role. Goose and the
// backfill checkpoint serialize concurrent Helm hook jobs. Retries resume from the last commit.
func Migrate(ctx context.Context, url string, logger *slog.Logger) error {
	backoff := time.Second
	for attempt := 1; ; attempt++ {
		err := migrateOnce(ctx, url, logger)
		var pgErr *pgconn.PgError
		if err == nil || !errors.As(err, &pgErr) || (pgErr.Code != "55P03" && pgErr.Code != "40P01") || attempt == lockRetries {
			return err
		}
		logger.WarnContext(ctx, "migration lock conflict; retrying", slog.String("sqlstate", pgErr.Code), slog.Int("attempt", attempt), slog.Duration("backoff", backoff))
		select {
		case <-ctx.Done():
			return errors.Join(err, ctx.Err())
		case <-time.After(backoff):
		}
		backoff *= 2
	}
}

func migrateOnce(ctx context.Context, url string, logger *slog.Logger) error {
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
	return backfillResourceState(ctx, db, logger)
}
