package postgres

import "github.com/jackc/pgx/v5/pgxpool"

// RawPool exposes the underlying pool to this package's tests only, so they can simulate code that
// bypasses the tenant-safe API (session-level SET) and inspect connection state.
func RawPool(p *Pool) *pgxpool.Pool { return p.pool }
