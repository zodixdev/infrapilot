//go:build integration

package service_test

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/zodixdev/infrapilot/services/control-plane/internal/domain"
	"github.com/zodixdev/infrapilot/services/control-plane/internal/service"
)

// TestCreateAPIKey_WaitsForConcurrentOrgDelete_Integration: an organization delete (soft delete + revoke of every
// key) is in progress when CreateAPIKey runs. The share lock on the live organization makes CreateAPIKey wait for
// the delete and then fail with NotFound; without it the new key would commit after the revoke and stay live in a
// deleted organization.
func TestCreateAPIKey_WaitsForConcurrentOrgDelete_Integration(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	alice := e.user("alice")
	org := e.org(alice, "Acme")
	project := e.project(alice, org, "web")
	_, existing := e.key(alice, org, project, domain.ScopeRead)

	type result struct {
		key service.CreatedAPIKey
		err error
	}
	done := make(chan result, 1)
	err := e.db.App.WithTx(ctx, org, func(ctx context.Context, tx pgx.Tx) error {
		// The statements of DeleteOrganization, held uncommitted.
		if _, err := tx.Exec(ctx, `UPDATE organizations SET deleted_at = now(), updated_at = now()
			WHERE id = $1 AND organization_id = $1 AND deleted_at IS NULL`, org); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE api_keys SET revoked_at = now() WHERE organization_id = $1 AND revoked_at IS NULL`, org); err != nil {
			return err
		}
		go func() {
			k, err := e.svc.CreateAPIKey(context.WithoutCancel(ctx), alice, org,
				service.CreateAPIKeyInput{ProjectID: project, Name: "late", Scopes: []domain.Scope{domain.ScopeRead}})
			done <- result{k, err}
		}()
		// Commit only once CreateAPIKey is blocked on the organization row lock (well inside the 5 s lock_timeout).
		require.Eventually(t, func() bool {
			select {
			case r := <-done:
				done <- r
				return true // finished without waiting: the assertions below fail
			default:
			}
			var waiting int
			err := e.db.App.WithTx(ctx, org, func(ctx context.Context, q pgx.Tx) error {
				return q.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
					WHERE wait_event_type = 'Lock' AND query ILIKE '%FROM organizations%FOR SHARE%'`).Scan(&waiting)
			})
			return err == nil && waiting == 1
		}, 3*time.Second, 5*time.Millisecond)
		return nil
	})
	require.NoError(t, err)

	var r result
	select {
	case r = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("CreateAPIKey did not finish after the delete committed")
	}
	require.ErrorIs(t, r.err, domain.ErrNotFound, "a key cannot be created in an organization deleted meanwhile")
	require.Empty(t, r.key.Secret)

	var live int
	require.NoError(t, e.db.App.WithTx(ctx, org, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM api_keys WHERE organization_id = $1 AND revoked_at IS NULL`, org).Scan(&live)
	}))
	require.Zero(t, live, "no live API key survives the organization delete")
	h := sha256.Sum256([]byte(existing))
	_, err = e.svc.AuthenticateAPIKey(ctx, h[:])
	require.ErrorIs(t, err, domain.ErrUnauthenticated)
}
