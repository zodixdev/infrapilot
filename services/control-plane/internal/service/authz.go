package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/zodixdev/infrapilot/pkg/postgres"
	"github.com/zodixdev/infrapilot/services/control-plane/internal/domain"
	"github.com/zodixdev/infrapilot/services/control-plane/internal/store/db"
)

// Decision is the answer of CheckPermission.
type Decision struct {
	Allowed bool
	Role    domain.Role // users that are members; empty otherwise
}

// CheckPermission decides whether the actor holds perm in org (and project). Unknown organizations, non-members
// and keys of other tenants are "not allowed", never an error, so callers cannot probe for existence (ADR-0022).
func (s *Service) CheckPermission(ctx context.Context, a Actor, org uuid.UUID, perm domain.Permission, project uuid.UUID) (Decision, error) {
	var v domain.Validator
	v.Check(org != uuid.Nil, "organization_id", "is required")
	v.Check(isPermission(perm), "permission", "is not a known permission")
	if err := v.Err(); err != nil {
		return Decision{}, err
	}
	var out Decision
	err := s.pool.WithTx(ctx, org, func(ctx context.Context, tx pgx.Tx) error {
		g, err := authorize(ctx, db.New(tx), a, org, perm, project)
		out.Role = g.role
		switch {
		case err == nil:
			out.Allowed = true
			return nil
		case errors.Is(err, domain.ErrNotFound), errors.Is(err, domain.ErrPermissionDenied):
			if errors.Is(err, domain.ErrNotFound) {
				out.Role = ""
			}
			return nil
		default:
			return err
		}
	}, postgres.ReadOnly())
	return out, err
}

func isPermission(p domain.Permission) bool {
	for _, x := range domain.Permissions {
		if x == p {
			return true
		}
	}
	return false
}

// ListAuditEvents lists an organization's audit log, newest first (audit.read).
func (s *Service) ListAuditEvents(ctx context.Context, a Actor, org uuid.UUID, size int32, token string) ([]domain.AuditEvent, string, error) {
	var v domain.Validator
	page := domain.ParsePage(&v, size, token)
	if err := v.Err(); err != nil {
		return nil, "", err
	}
	before := page.Cursor
	if before == uuid.Nil {
		before = uuid.Max
	}
	var rows []db.AuditEvent
	err := s.pool.WithTx(ctx, org, func(ctx context.Context, tx pgx.Tx) error {
		q := db.New(tx)
		if _, err := authorize(ctx, q, a, org, domain.PermAuditRead, uuid.Nil); err != nil {
			return err
		}
		var err error
		rows, err = q.ListAuditEvents(ctx, db.ListAuditEventsParams{OrganizationID: org, Before: before, Lim: page.Limit()})
		return err
	}, postgres.ReadOnly())
	if err != nil {
		return nil, "", err
	}
	rows, more := domain.Trim(page, rows)
	out := make([]domain.AuditEvent, len(rows))
	var last uuid.UUID
	for i := range rows {
		r := &rows[i]
		details := map[string]string{}
		if err := json.Unmarshal(r.Details, &details); err != nil {
			return nil, "", fmt.Errorf("audit event %s details: %w", r.ID, err)
		}
		out[i] = domain.AuditEvent{ID: r.ID, OrganizationID: r.OrganizationID, ActorKind: domain.ActorKind(r.ActorKind), ActorID: r.ActorID,
			Action: r.Action, TargetType: r.TargetType, TargetID: r.TargetID, Details: details, RequestID: r.RequestID,
			TraceID: r.TraceID, CreatedAt: r.CreatedAt}
		last = r.ID
	}
	return out, domain.NextToken(more, last), nil
}
