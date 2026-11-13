package postgres

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// OpenSQL returns a database/sql handle with the same connection settings as Open (timeouts, application
// name). It exists for libraries that require database/sql, such as the goose migration provider; request
// paths use Pool. Typically called with MigrationConfig. The caller closes it.
func OpenSQL(cfg Config) (*sql.DB, error) {
	if cfg.URL == "" {
		return nil, errors.New("postgres: URL is required")
	}
	cfg = cfg.Defaults()
	cc, err := pgx.ParseConfig(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse url: %w", err)
	}
	applyConnConfig(cc, cfg)
	db := stdlib.OpenDB(*cc)
	db.SetMaxOpenConns(int(cfg.MaxConns))
	db.SetMaxIdleConns(int(cfg.MaxConns))
	db.SetConnMaxLifetime(cfg.MaxConnLifetime)
	return db, nil
}
