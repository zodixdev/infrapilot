//go:build integration

package service_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/goleak"

	"github.com/zodixdev/infrapilot/services/control-plane/internal/service"
)

type capturedState struct {
	Revision    int64
	ProjectID   uuid.UUID
	Status      int16
	TraceParent string
}

func TestResourceStateCapture_Integration(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	owner := e.user("owner")
	org := e.org(owner, "Infrastructure")
	shop := e.project(owner, org, "shop")
	admin := e.project(owner, org, "admin")
	migrator, err := pgx.Connect(ctx, e.db.MigratorURL)
	require.NoError(t, err)
	t.Cleanup(func() { _ = migrator.Close(context.WithoutCancel(ctx)) })

	captured := func(resource uuid.UUID) []capturedState {
		t.Helper()
		rows, queryErr := migrator.Query(ctx, `SELECT revision, project_id, status, trace_parent
			FROM resource_state_outbox WHERE routing_key = $1 ORDER BY revision`, org.String()+"/"+resource.String())
		require.NoError(t, queryErr)
		states, collectErr := pgx.CollectRows(rows, pgx.RowToStructByPos[capturedState])
		require.NoError(t, collectErr)
		return states
	}

	traceID, err := trace.TraceIDFromHex("410c74e72cda0045f893f51b0ad1803a")
	require.NoError(t, err)
	spanID, err := trace.SpanIDFromHex("06e246cfccfa02d3")
	require.NoError(t, err)
	tracedCtx := trace.ContextWithSpanContext(ctx, trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled,
	}))
	input := service.CreateResourceInput{Kind: "service", Name: "web"}
	resource, err := e.svc.CreateResource(tracedCtx, owner, org, shop, input)
	require.NoError(t, err)
	independent, err := e.svc.CreateResource(ctx, owner, org, admin, input)
	require.NoError(t, err)
	states := captured(resource.ID)
	require.Len(t, states, 1)
	require.Equal(t, shop, states[0].ProjectID)
	require.EqualValues(t, 1, states[0].Status)
	require.Equal(t, "00-"+traceID.String()+"-"+spanID.String()+"-01", states[0].TraceParent)
	states = captured(independent.ID)
	require.Len(t, states, 1)
	require.Empty(t, states[0].TraceParent)

	rolledBack := uuid.New()
	rollback := errors.New("test rollback")
	err = e.db.App.WithTx(ctx, org, func(ctx context.Context, tx pgx.Tx) error {
		if _, insertErr := tx.Exec(ctx, `INSERT INTO resources (id, organization_id, project_id, kind, name)
			VALUES ($1, $2, $3, 'service', 'rolled-back')`, rolledBack, org, shop); insertErr != nil {
			return insertErr
		}
		return rollback
	})
	require.ErrorIs(t, err, rollback)
	require.Empty(t, captured(rolledBack))

	newName := "frontend"
	_, err = e.svc.UpdateResource(ctx, owner, org, shop, resource.ID, service.UpdateResourceInput{Name: &newName})
	require.NoError(t, err)
	require.Len(t, captured(resource.ID), 1)

	require.NoError(t, e.svc.DeleteProject(ctx, owner, org, admin))
	require.NoError(t, e.svc.DeleteOrganization(ctx, owner, org))
	_, err = migrator.Exec(ctx, `UPDATE organizations SET deleted_at = NULL WHERE id = $1`, org)
	require.NoError(t, err)
	require.NoError(t, e.svc.DeleteResource(ctx, owner, org, shop, resource.ID))
	require.NoError(t, e.db.Batch.WithSystemTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM resources WHERE id = $1`, resource.ID)
		return err
	}))
	checkStatuses(t, captured(resource.ID), 1, 2, 1, 2, 3)
	checkStatuses(t, captured(independent.ID), 1, 2)
}

func TestResourceStateLegacyInsertWaitsForOrganization_Integration(t *testing.T) {
	baseline := goleak.IgnoreCurrent()
	t.Cleanup(func() { goleak.VerifyNone(t, baseline) })
	e := newEnv(t)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	owner := e.user("owner")
	org := e.org(owner, "Infrastructure")
	project := e.project(owner, org, "scheduling")
	conn, err := pgx.Connect(ctx, e.db.MigratorURL)
	require.NoError(t, err)
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	deletion, err := conn.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = deletion.Rollback(context.WithoutCancel(ctx)) }()
	_, err = deletion.Exec(ctx, `UPDATE organizations SET deleted_at = now() WHERE id = $1`, org)
	require.NoError(t, err)

	// M02 only locked the project before INSERT. The trigger must hold the late insert until it can
	// read the latest organization state, even when old replicas are still accepting requests.
	resource := uuid.New()
	writerPID := make(chan uint32, 1)
	writerResult := make(chan error, 1)
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		writerResult <- e.db.App.WithTx(ctx, org, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT id FROM projects WHERE id = $1 FOR SHARE`, project); err != nil {
				return err
			}
			writerPID <- tx.Conn().PgConn().PID()
			_, err := tx.Exec(ctx, `INSERT INTO resources (id, organization_id, project_id, kind, name)
				VALUES ($1, $2, $3, 'service', 'legacy-writer')`, resource, org, project)
			return err
		})
	}()
	defer func() {
		cancel()
		<-writerDone
	}()
	var pid uint32
	select {
	case pid = <-writerPID:
	case err := <-writerResult:
		t.Fatalf("legacy writer ended before INSERT: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	wait, stop := context.WithTimeout(ctx, 3*time.Second)
	defer stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		var blocked bool
		require.NoError(t, deletion.QueryRow(wait, `SELECT pg_backend_pid() = ANY(pg_blocking_pids($1))`, pid).Scan(&blocked))
		if blocked {
			break
		}
		select {
		case <-ticker.C:
		case <-wait.Done():
			t.Fatal("legacy insert did not wait for the organization row")
		}
	}
	require.NoError(t, deletion.Commit(ctx))
	require.NoError(t, <-writerResult)

	var status int16
	require.NoError(t, conn.QueryRow(ctx, `SELECT status FROM resource_state_outbox WHERE routing_key = $1`, org.String()+"/"+resource.String()).Scan(&status))
	require.EqualValues(t, 2, status)
}

func checkStatuses(t *testing.T, states []capturedState, want ...int16) {
	t.Helper()
	require.Len(t, states, len(want))
	var previous int64
	for i, state := range states {
		require.Equal(t, want[i], state.Status)
		require.Greater(t, state.Revision, previous)
		previous = state.Revision
	}
}
