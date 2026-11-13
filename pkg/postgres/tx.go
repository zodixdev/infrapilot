package postgres

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ErrNoTenant is returned when WithTx is called with the nil UUID.
var ErrNoTenant = errors.New("postgres: tenant id is required")

// ErrNoUser is returned by WithUserTx for the nil user id.
var ErrNoUser = errors.New("postgres: user id is required")

// TxOption adjusts transaction characteristics.
type TxOption func(*pgx.TxOptions)

// ReadOnly marks the transaction read-only (lets Postgres reject accidental writes).
func ReadOnly() TxOption { return func(o *pgx.TxOptions) { o.AccessMode = pgx.ReadOnly } }

// Isolation sets the isolation level (default READ COMMITTED). With Serializable or RepeatableRead the
// caller must retry the whole fn on SQLSTATE 40001/40P01, and only if fn has no side effects outside
// the transaction.
func Isolation(level pgx.TxIsoLevel) TxOption { return func(o *pgx.TxOptions) { o.IsoLevel = level } }

// WithTx runs fn inside a transaction scoped to orgID: `app.org_id` is set with `set_config(..., true)`
// right after BEGIN, so RLS policies (ADR-0018) see it for this transaction only. The transaction is
// committed when fn returns nil and rolled back when it returns an error or panics (the panic continues).
// It refuses to run on a BYPASSRLS pool, where the tenant context would have no effect.
func (p *Pool) WithTx(ctx context.Context, orgID uuid.UUID, fn func(ctx context.Context, tx pgx.Tx) error, opts ...TxOption) error {
	if orgID == uuid.Nil {
		return ErrNoTenant
	}
	if p.bypassRLS {
		return ErrBypassRLS
	}
	return p.runTx(ctx, orgID.String(), "", fn, opts)
}

// WithUserTx runs fn in a READ ONLY transaction scoped to a user instead of a tenant: `app.user_id` is set
// and `app.org_id` cleared, so only the SELECT-only user policies of ADR-0023 can match rows (e.g. "the
// organizations I belong to"). Writes are impossible: the transaction is read-only and tenant policies'
// WITH CHECK needs `app.org_id`. Like WithTx it refuses BYPASSRLS pools.
func (p *Pool) WithUserTx(ctx context.Context, userID uuid.UUID, fn func(ctx context.Context, tx pgx.Tx) error, opts ...TxOption) error {
	if userID == uuid.Nil {
		return ErrNoUser
	}
	if p.bypassRLS {
		return ErrBypassRLS
	}
	// Concat copies: appending to opts could write into the caller's backing array.
	return p.runTx(ctx, "", userID.String(), fn, slices.Concat(opts, []TxOption{ReadOnly()}))
}

// WithSystemTx runs fn with the tenant and user contexts explicitly cleared. Use it for service-internal
// tables (outbox, processed_messages) and for the pre-tenant lookup tables of ADR-0018. Under the runtime role
// tenant tables return no rows, whatever state the pooled connection was left in.
func (p *Pool) WithSystemTx(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error, opts ...TxOption) error {
	return p.runTx(ctx, "", "", fn, opts)
}

func (p *Pool) runTx(ctx context.Context, orgID, userID string, fn func(ctx context.Context, tx pgx.Tx) error, opts []TxOption) (err error) {
	txOpts := pgx.TxOptions{IsoLevel: pgx.ReadCommitted}
	for _, o := range opts {
		o(&txOpts)
	}
	tx, err := p.pool.BeginTx(ctx, txOpts)
	if err != nil {
		return fmt.Errorf("postgres: begin: %w", err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		// Runs on error and while a panic unwinds (the panic then continues with its original stack).
		// The rollback outlives request cancellation but is bounded, so a black-holed connection cannot
		// pin the goroutine; pgx discards the connection if the rollback fails.
		rbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), p.cfg.RollbackTimeout)
		defer cancel()
		if rbErr := tx.Rollback(rbCtx); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
			err = errors.Join(err, fmt.Errorf("postgres: rollback: %w", rbErr))
		}
	}()
	// Always set both values (empty when unused) so a session-level value left on the connection by anything
	// outside this package can never apply. One statement, one round trip.
	if _, err := tx.Exec(ctx, "SELECT set_config('app.org_id', $1, true), set_config('app.user_id', $2, true)", orgID, userID); err != nil {
		return fmt.Errorf("postgres: set tenant: %w", err)
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
