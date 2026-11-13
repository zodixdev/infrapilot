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

// Migrate applies every pending migration as the migrator role (MigrationConfig: one connection, no statement
// timeout, 3 s lock timeout). goose serializes concurrent runners with a Postgres session advisory lock, so
// parallel Helm hook jobs are safe. Each migration commits on its own, so a retry resumes after the last
// applied one.
func Migrate(ctx context.Context, url string, logger *slog.Logger) error {
	backoff := time.Second
	for attempt := 1; ; attempt++ {
		err := migrateOnce(ctx, url, logger)
		var pgErr *pgconn.PgError
		if err == nil || !errors.As(err, &pgErr) || pgErr.Code != "55P03" || attempt == lockRetries {
			return err
		}
		logger.WarnContext(ctx, "migration hit lock_timeout; retrying", slog.Int("attempt", attempt), slog.Duration("backoff", backoff))
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
	return nil
}
