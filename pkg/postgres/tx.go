package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ErrNoTenant is returned when WithTx is called with the nil UUID.
var ErrNoTenant = errors.New("postgres: tenant id is required")

// TxOption adjusts transaction characteristics.
type TxOption func(*pgx.TxOptions)

// ReadOnly marks the transaction read-only (lets Postgres reject accidental writes).
func ReadOnly() TxOption { return func(o *pgx.TxOptions) { o.AccessMode = pgx.ReadOnly } }

// Isolation sets the isolation level (default READ COMMITTED).
func Isolation(level pgx.TxIsoLevel) TxOption { return func(o *pgx.TxOptions) { o.IsoLevel = level } }

// WithTx runs fn inside a transaction scoped to orgID: `app.org_id` is set with `set_config(..., true)`
// immediately after BEGIN, so RLS policies (ADR-0018) see it for this transaction only. The transaction
// is committed when fn returns nil, rolled back when it returns an error, and rolled back on panic
// before the panic is re-raised.
func (p *Pool) WithTx(ctx context.Context, orgID uuid.UUID, fn func(ctx context.Context, tx pgx.Tx) error, opts ...TxOption) error {
	if orgID == uuid.Nil {
		return ErrNoTenant
	}
	return p.runTx(ctx, &orgID, fn, opts)
}

// WithSystemTx runs fn without a tenant context. Use it only for service-internal tables (outbox,
// processed_messages). Under the runtime role, tenant tables return no rows in such a transaction.
func (p *Pool) WithSystemTx(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error, opts ...TxOption) error {
	return p.runTx(ctx, nil, fn, opts)
}

func (p *Pool) runTx(ctx context.Context, orgID *uuid.UUID, fn func(ctx context.Context, tx pgx.Tx) error, opts []TxOption) (err error) {
	txOpts := pgx.TxOptions{IsoLevel: pgx.ReadCommitted}
	for _, o := range opts {
		o(&txOpts)
	}
	tx, err := p.BeginTx(ctx, txOpts)
	if err != nil {
		return fmt.Errorf("postgres: begin: %w", err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		// Rollback on error or panic. Use a context that survives cancellation so the connection is
		// returned to the pool in a clean state.
		rbErr := tx.Rollback(context.WithoutCancel(ctx))
		if r := recover(); r != nil {
			panic(r)
		}
		if rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
			err = errors.Join(err, fmt.Errorf("postgres: rollback: %w", rbErr))
		}
	}()
	if orgID != nil {
		if _, err := tx.Exec(ctx, "SELECT set_config('app.org_id', $1, true)", orgID.String()); err != nil {
			return fmt.Errorf("postgres: set tenant: %w", err)
		}
	}
	if err := fn(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres: commit: %w", err)
	}
	committed = true
	return nil
}
