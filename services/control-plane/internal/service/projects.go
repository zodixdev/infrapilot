package service

import (
	"context"
	"fmt"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/zodixdev/infrapilot/pkg/postgres"
	"github.com/zodixdev/infrapilot/services/control-plane/internal/domain"
	"github.com/zodixdev/infrapilot/services/control-plane/internal/store/db"
)

var projectConflicts = map[string]string{"projects_slug_key": "a project with this slug already exists"}

// CreateProjectInput is the request of CreateProject.
type CreateProjectInput struct {
	Name        string `json:"name"`
	Slug        string `json:"slug"`
	Description string `json:"description"`
}

// CreateProject creates a project (project.write).
func (s *Service) CreateProject(ctx context.Context, a Actor, org uuid.UUID, in CreateProjectInput) (domain.Project, error) {
	var v domain.Validator
	domain.CheckName(&v, "name", in.Name)
	domain.CheckSlug(&v, "slug", in.Slug)
	domain.CheckDescription(&v, "description", in.Description)
	if err := v.Err(); err != nil {
		return domain.Project{}, err
	}
	idem, idemErr := newIdempotency(ctx, "CreateProject", in)
	if idemErr != nil {
		return domain.Project{}, idemErr
	}
	p, err := createIdempotent(ctx, s, org, a, idem,
		func(ctx context.Context, q *db.Queries) (domain.Project, uuid.UUID, error) {
			if _, err := authorize(ctx, q, a, org, domain.PermProjectWrite, uuid.Nil); err != nil {
				return domain.Project{}, uuid.Nil, err
			}
			row, err := q.InsertProject(ctx, db.InsertProjectParams{ID: newID(), OrganizationID: org, Name: in.Name, Slug: in.Slug, Description: in.Description})
			if err != nil {
				return domain.Project{}, uuid.Nil, err
			}
			err = audit(ctx, q, org, a, "project.created", "project", row.ID.String(), map[string]string{"slug": row.Slug})
			return toProject(&row), row.ID, err
		},
		func(ctx context.Context, q *db.Queries, id uuid.UUID) (domain.Project, error) {
			if _, err := authorize(ctx, q, a, org, domain.PermProjectRead, id); err != nil {
				return domain.Project{}, err
			}
			row, err := q.GetProject(ctx, db.GetProjectParams{OrganizationID: org, ID: id})
			return toProject(&row), notFound(err, "project %s", id)
		})
	return p, conflictOn(err, projectConflicts)
}

// GetProject returns a project (project.read; API keys only their own project).
func (s *Service) GetProject(ctx context.Context, a Actor, org, id uuid.UUID) (domain.Project, error) {
	var out domain.Project
	err := s.pool.WithTx(ctx, org, func(ctx context.Context, tx pgx.Tx) error {
		q := db.New(tx)
		if _, err := authorize(ctx, q, a, org, domain.PermProjectRead, id); err != nil {
			return err
		}
		row, err := q.GetProject(ctx, db.GetProjectParams{OrganizationID: org, ID: id})
		out = toProject(&row)
		return notFound(err, "project %s", id)
	}, postgres.ReadOnly())
	return out, err
}

// ListProjects lists the organization's projects (members with project.read; API keys see only their project).
func (s *Service) ListProjects(ctx context.Context, a Actor, org uuid.UUID, size int32, token string) ([]domain.Project, string, error) {
	var v domain.Validator
	page := domain.ParsePage(&v, size, token)
	if err := v.Err(); err != nil {
		return nil, "", err
	}
	var rows []db.Project
	err := s.pool.WithTx(ctx, org, func(ctx context.Context, tx pgx.Tx) error {
		q := db.New(tx)
		if a.Kind == domain.ActorAPIKey {
			// A key reads only the project it is bound to.
			g, err := authorize(ctx, q, a, org, domain.PermOrgRead, uuid.Nil)
			if err != nil {
				return err
			}
			if page.Cursor != uuid.Nil {
				return nil // the single-row page was already returned
			}
			row, err := q.GetProject(ctx, db.GetProjectParams{OrganizationID: org, ID: g.keyProject})
			if err == nil {
				rows = []db.Project{row}
			}
			return notFound(err, "project %s", g.keyProject)
		}
		if _, err := authorize(ctx, q, a, org, domain.PermProjectRead, uuid.Nil); err != nil {
			return err
		}
		var err error
		rows, err = q.ListProjects(ctx, db.ListProjectsParams{OrganizationID: org, After: page.Cursor, Lim: page.Limit()})
		return err
	}, postgres.ReadOnly())
	if err != nil {
		return nil, "", err
	}
	rows, more := domain.Trim(page, rows)
	out := make([]domain.Project, len(rows))
	var last uuid.UUID
	for i := range rows {
		r := &rows[i]
		out[i], last = toProject(r), r.ID
	}
	return out, domain.NextToken(more, last), nil
}

// UpdateProjectInput carries optional fields; nil means unchanged.
type UpdateProjectInput struct {
	Name, Slug, Description *string
}

// UpdateProject changes a project (project.write).
func (s *Service) UpdateProject(ctx context.Context, a Actor, org, id uuid.UUID, in UpdateProjectInput) (domain.Project, error) {
	var v domain.Validator
	if in.Name != nil {
		domain.CheckName(&v, "name", *in.Name)
	}
	if in.Slug != nil {
		domain.CheckSlug(&v, "slug", *in.Slug)
	}
	if in.Description != nil {
		domain.CheckDescription(&v, "description", *in.Description)
	}
	if err := v.Err(); err != nil {
		return domain.Project{}, err
	}
	var out domain.Project
	err := s.pool.WithTx(ctx, org, func(ctx context.Context, tx pgx.Tx) error {
		q := db.New(tx)
		if _, err := authorize(ctx, q, a, org, domain.PermProjectWrite, uuid.Nil); err != nil {
			return err
		}
		row, err := q.UpdateProject(ctx, db.UpdateProjectParams{OrganizationID: org, ID: id, Name: in.Name, Slug: in.Slug, Description: in.Description})
		if err != nil {
			return notFound(err, "project %s", id)
		}
		out = toProject(&row)
		changes := map[string]string{}
		for field, v := range map[string]*string{"name": in.Name, "slug": in.Slug, "description": in.Description} {
			if v != nil {
				changes[field] = *v
			}
		}
		if len(changes) == 0 {
			return nil
		}
		return audit(ctx, q, org, a, "project.updated", "project", id.String(), changes)
	})
	return out, conflictOn(err, projectConflicts)
}

// DeleteProject soft-deletes a project with its resources and revokes its API keys, atomically.
func (s *Service) DeleteProject(ctx context.Context, a Actor, org, id uuid.UUID) error {
	return s.pool.WithTx(ctx, org, func(ctx context.Context, tx pgx.Tx) error {
		q := db.New(tx)
		if _, err := authorize(ctx, q, a, org, domain.PermProjectWrite, uuid.Nil); err != nil {
			return err
		}
		n, err := q.SoftDeleteProject(ctx, db.SoftDeleteProjectParams{OrganizationID: org, ID: id})
		if err != nil {
			return fmt.Errorf("delete project: %w", err)
		}
		if n == 0 {
			return domain.NotFoundf("project %s", id)
		}
		if err = q.SoftDeleteProjectResources(ctx, db.SoftDeleteProjectResourcesParams{OrganizationID: org, ProjectID: id}); err != nil {
			return fmt.Errorf("delete project resources: %w", err)
		}
		revoked, err := q.RevokeProjectAPIKeys(ctx, db.RevokeProjectAPIKeysParams{OrganizationID: org, ProjectID: id})
		if err != nil {
			return fmt.Errorf("revoke project keys: %w", err)
		}
		return audit(ctx, q, org, a, "project.deleted", "project", id.String(), map[string]string{"revoked_api_keys": strconv.FormatInt(revoked, 10)})
	})
}

// lockLiveProject takes a share lock on a live project so a concurrent delete cannot orphan the child being
// created (the soft delete's row lock waits for it, and vice versa).
func lockLiveProject(ctx context.Context, q *db.Queries, org, id uuid.UUID) error {
	_, err := q.LockLiveProject(ctx, db.LockLiveProjectParams{OrganizationID: org, ID: id})
	return notFound(err, "project %s", id)
}

func toProject(r *db.Project) domain.Project {
	return domain.Project{ID: r.ID, OrganizationID: r.OrganizationID, Name: r.Name, Slug: r.Slug,
		Description: r.Description, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
}
