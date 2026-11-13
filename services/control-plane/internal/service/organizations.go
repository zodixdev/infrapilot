package service

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/zodixdev/infrapilot/pkg/postgres"
	"github.com/zodixdev/infrapilot/services/control-plane/internal/domain"
	"github.com/zodixdev/infrapilot/services/control-plane/internal/store/db"
)

// orgIDNamespace derives deterministic organization ids for idempotent creates (ADR-0024).
var orgIDNamespace = uuid.MustParse("5d0f6c1e-8a1b-4f43-9a8e-6f1f0c2b7a10")

// CreateOrganizationInput is the request of CreateOrganization.
type CreateOrganizationInput struct {
	Name string `json:"name"`
}

// CreateOrganization creates an organization owned by the calling user.
func (s *Service) CreateOrganization(ctx context.Context, a Actor, in CreateOrganizationInput) (domain.Organization, error) {
	if a.Kind != domain.ActorUser {
		return domain.Organization{}, domain.Deniedf("only users can create organizations")
	}
	var v domain.Validator
	domain.CheckName(&v, "name", in.Name)
	if err := v.Err(); err != nil {
		return domain.Organization{}, err
	}
	idem, idemErr := newIdempotency(ctx, "CreateOrganization", in)
	if idemErr != nil {
		return domain.Organization{}, idemErr
	}
	orgID := newID()
	if idem != nil {
		// No tenant exists yet: derive the id from (principal, key) so a replay finds its record (ADR-0024).
		orgID = uuid.NewSHA1(orgIDNamespace, append(a.ID[:], []byte(idem.key)...))
	}
	org, err := createIdempotent(ctx, s, orgID, a, idem,
		func(ctx context.Context, q *db.Queries) (domain.Organization, uuid.UUID, error) {
			row, err := q.InsertOrganization(ctx, db.InsertOrganizationParams{ID: orgID, Name: in.Name})
			if err != nil {
				return domain.Organization{}, uuid.Nil, err
			}
			if err := q.InsertMembership(ctx, db.InsertMembershipParams{OrganizationID: orgID, UserID: a.ID, Role: string(domain.RoleOwner), CreatedBy: &a.ID}); err != nil {
				return domain.Organization{}, uuid.Nil, err
			}
			if err := audit(ctx, q, orgID, a, "org.created", "organization", orgID.String(), map[string]string{"name": in.Name}); err != nil {
				return domain.Organization{}, uuid.Nil, err
			}
			return toOrganization(&row), orgID, nil
		},
		func(ctx context.Context, q *db.Queries, id uuid.UUID) (domain.Organization, error) {
			// The creator may have left or been removed since: a replay is a read and needs org.read.
			if _, err := authorize(ctx, q, a, id, domain.PermOrgRead, uuid.Nil); err != nil {
				return domain.Organization{}, err
			}
			row, err := q.GetOrganization(ctx, id)
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.Organization{}, domain.NotFoundf("organization %s was deleted", id)
			}
			return toOrganization(&row), err
		})
	if idem != nil && constraint(err) == "organizations_pkey" {
		// The derived id belongs to an organization created with this key more than 24 h ago (its record expired).
		return domain.Organization{}, domain.Conflictf("this Idempotency-Key was already used for an organization; use a new key")
	}
	return org, err
}

// GetOrganization returns an organization the actor can read.
func (s *Service) GetOrganization(ctx context.Context, a Actor, org uuid.UUID) (domain.Organization, error) {
	var out domain.Organization
	err := s.pool.WithTx(ctx, org, func(ctx context.Context, tx pgx.Tx) error {
		q := db.New(tx)
		if _, err := authorize(ctx, q, a, org, domain.PermOrgRead, uuid.Nil); err != nil {
			return err
		}
		row, err := q.GetOrganization(ctx, org)
		if err != nil {
			return notFound(err, "organization %s", org)
		}
		out = toOrganization(&row)
		return nil
	}, postgres.ReadOnly())
	return out, err
}

// ListMyOrganizations lists the calling user's organizations with their role (ADR-0023).
func (s *Service) ListMyOrganizations(ctx context.Context, a Actor, size int32, token string) ([]domain.OrganizationWithRole, string, error) {
	if a.Kind != domain.ActorUser {
		return nil, "", domain.Deniedf("only users have organization memberships")
	}
	var v domain.Validator
	page := domain.ParsePage(&v, size, token)
	if err := v.Err(); err != nil {
		return nil, "", err
	}
	var rows []db.ListOrganizationsForUserRow
	err := s.pool.WithUserTx(ctx, a.ID, func(ctx context.Context, tx pgx.Tx) (err error) {
		rows, err = db.New(tx).ListOrganizationsForUser(ctx, db.ListOrganizationsForUserParams{UserID: a.ID, After: page.Cursor, Lim: page.Limit()})
		return err
	})
	if err != nil {
		return nil, "", fmt.Errorf("list organizations: %w", err)
	}
	rows, more := domain.Trim(page, rows)
	out := make([]domain.OrganizationWithRole, len(rows))
	for i := range rows {
		r := &rows[i]
		out[i] = domain.OrganizationWithRole{
			Organization: domain.Organization{ID: r.ID, Name: r.Name, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt},
			Role:         domain.Role(r.Role),
		}
	}
	var last uuid.UUID
	if len(rows) > 0 {
		last = rows[len(rows)-1].ID
	}
	return out, domain.NextToken(more, last), nil
}

// UpdateOrganizationInput carries optional fields; nil means unchanged.
type UpdateOrganizationInput struct {
	Name *string
}

// UpdateOrganization renames an organization.
func (s *Service) UpdateOrganization(ctx context.Context, a Actor, org uuid.UUID, in UpdateOrganizationInput) (domain.Organization, error) {
	var v domain.Validator
	if in.Name != nil {
		domain.CheckName(&v, "name", *in.Name)
	}
	if err := v.Err(); err != nil {
		return domain.Organization{}, err
	}
	var out domain.Organization
	err := s.pool.WithTx(ctx, org, func(ctx context.Context, tx pgx.Tx) error {
		q := db.New(tx)
		if _, err := authorize(ctx, q, a, org, domain.PermOrgUpdate, uuid.Nil); err != nil {
			return err
		}
		row, err := q.UpdateOrganization(ctx, db.UpdateOrganizationParams{ID: org, Name: in.Name})
		if err != nil {
			return notFound(err, "organization %s", org)
		}
		out = toOrganization(&row)
		if in.Name != nil {
			return audit(ctx, q, org, a, "org.updated", "organization", org.String(), map[string]string{"name": *in.Name})
		}
		return nil
	})
	return out, err
}

// DeleteOrganization soft-deletes an organization and revokes its API keys (ADR-0027).
func (s *Service) DeleteOrganization(ctx context.Context, a Actor, org uuid.UUID) error {
	return s.pool.WithTx(ctx, org, func(ctx context.Context, tx pgx.Tx) error {
		q := db.New(tx)
		if _, err := authorize(ctx, q, a, org, domain.PermOrgDelete, uuid.Nil); err != nil {
			return err
		}
		n, err := q.SoftDeleteOrganization(ctx, org)
		if err != nil {
			return fmt.Errorf("delete organization: %w", err)
		}
		if n == 0 {
			return domain.NotFoundf("organization %s", org)
		}
		revoked, err := q.RevokeOrganizationAPIKeys(ctx, org)
		if err != nil {
			return fmt.Errorf("delete organization: revoke keys: %w", err)
		}
		return audit(ctx, q, org, a, "org.deleted", "organization", org.String(), map[string]string{"revoked_api_keys": strconv.FormatInt(revoked, 10)})
	})
}

func toOrganization(r *db.Organization) domain.Organization {
	return domain.Organization{ID: r.ID, Name: r.Name, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
}

// notFound maps pgx.ErrNoRows to a domain NotFound with a caller-safe message.
func notFound(err error, format string, args ...any) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NotFoundf(format, args...)
	}
	return err
}
