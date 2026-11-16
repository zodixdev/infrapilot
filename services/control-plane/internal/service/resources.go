package service

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/zodixdev/infrapilot/pkg/postgres"
	"github.com/zodixdev/infrapilot/services/control-plane/internal/domain"
	"github.com/zodixdev/infrapilot/services/control-plane/internal/store/db"
)

var resourceConflicts = map[string]string{"resources_name_key": "a resource with this name already exists in the project"}

// CreateResourceInput is the request of CreateResource.
type CreateResourceInput struct {
	Kind   domain.ResourceKind `json:"kind"`
	Name   string              `json:"name"`
	Labels map[string]string   `json:"labels"`
}

func checkKind(v *domain.Validator, k domain.ResourceKind) {
	v.Check(k.Valid(), "kind", "must be one of host, service, endpoint, database, queue, container, other")
}

// CreateResource adds a resource to a project (resource.write).
func (s *Service) CreateResource(ctx context.Context, a Actor, org, project uuid.UUID, in CreateResourceInput) (domain.Resource, error) {
	var v domain.Validator
	checkKind(&v, in.Kind)
	domain.CheckName(&v, "name", in.Name)
	domain.CheckLabels(&v, "labels", in.Labels)
	if err := v.Err(); err != nil {
		return domain.Resource{}, err
	}
	labels, labelsErr := marshalLabels(in.Labels)
	if labelsErr != nil {
		return domain.Resource{}, labelsErr
	}
	idem, idemErr := newIdempotency(ctx, "CreateResource:"+project.String(), in)
	if idemErr != nil {
		return domain.Resource{}, idemErr
	}
	r, err := createIdempotent(ctx, s, org, a, idem,
		func(ctx context.Context, q *db.Queries) (domain.Resource, uuid.UUID, error) {
			if _, err := authorize(ctx, q, a, org, domain.PermResourceWrite, project); err != nil {
				return domain.Resource{}, uuid.Nil, err
			}
			if err := lockResourceParents(ctx, q, org, project); err != nil {
				return domain.Resource{}, uuid.Nil, err
			}
			row, err := q.InsertResource(ctx, db.InsertResourceParams{ID: newID(), OrganizationID: org, ProjectID: project,
				Kind: string(in.Kind), Name: in.Name, Labels: labels})
			if err != nil {
				return domain.Resource{}, uuid.Nil, err
			}
			res, err := toResource(&row)
			return res, row.ID, err
		},
		func(ctx context.Context, q *db.Queries, id uuid.UUID) (domain.Resource, error) {
			return getResource(ctx, q, a, org, project, id)
		})
	return r, conflictOn(err, resourceConflicts)
}

// GetResource returns a resource (resource.read).
func (s *Service) GetResource(ctx context.Context, a Actor, org, project, id uuid.UUID) (domain.Resource, error) {
	var out domain.Resource
	err := s.pool.WithTx(ctx, org, func(ctx context.Context, tx pgx.Tx) (err error) {
		out, err = getResource(ctx, db.New(tx), a, org, project, id)
		return err
	}, postgres.ReadOnly())
	return out, err
}

func getResource(ctx context.Context, q *db.Queries, a Actor, org, project, id uuid.UUID) (domain.Resource, error) {
	if _, err := authorize(ctx, q, a, org, domain.PermResourceRead, project); err != nil { // also checks the project is live
		return domain.Resource{}, err
	}
	row, err := q.GetResource(ctx, db.GetResourceParams{OrganizationID: org, ProjectID: project, ID: id})
	if err != nil {
		return domain.Resource{}, notFound(err, "resource %s", id)
	}
	return toResource(&row)
}

// ListResources lists a project's resources, optionally of one kind (resource.read).
func (s *Service) ListResources(ctx context.Context, a Actor, org, project uuid.UUID, kind domain.ResourceKind, size int32, token string) ([]domain.Resource, string, error) {
	var v domain.Validator
	page := domain.ParsePage(&v, size, token)
	if kind != "" {
		checkKind(&v, kind)
	}
	if err := v.Err(); err != nil {
		return nil, "", err
	}
	var rows []db.Resource
	err := s.pool.WithTx(ctx, org, func(ctx context.Context, tx pgx.Tx) error {
		q := db.New(tx)
		if _, err := authorize(ctx, q, a, org, domain.PermResourceRead, project); err != nil { // also checks the project is live
			return err
		}
		var err error
		if kind == "" {
			rows, err = q.ListResources(ctx, db.ListResourcesParams{OrganizationID: org, ProjectID: project, After: page.Cursor, Lim: page.Limit()})
		} else {
			rows, err = q.ListResourcesByKind(ctx, db.ListResourcesByKindParams{OrganizationID: org, ProjectID: project, Kind: string(kind), After: page.Cursor, Lim: page.Limit()})
		}
		return err
	}, postgres.ReadOnly())
	if err != nil {
		return nil, "", err
	}
	rows, more := domain.Trim(page, rows)
	out := make([]domain.Resource, len(rows))
	var last uuid.UUID
	for i := range rows {
		r := &rows[i]
		if out[i], err = toResource(r); err != nil {
			return nil, "", err
		}
		last = r.ID
	}
	return out, domain.NextToken(more, last), nil
}

// UpdateResourceInput carries optional fields; nil means unchanged (Labels nil keeps, empty map clears).
type UpdateResourceInput struct {
	Name   *string
	Labels map[string]string
}

// UpdateResource renames a resource or replaces its labels (resource.write).
func (s *Service) UpdateResource(ctx context.Context, a Actor, org, project, id uuid.UUID, in UpdateResourceInput) (domain.Resource, error) {
	var v domain.Validator
	if in.Name != nil {
		domain.CheckName(&v, "name", *in.Name)
	}
	var labels []byte
	if in.Labels != nil {
		domain.CheckLabels(&v, "labels", in.Labels)
	}
	if err := v.Err(); err != nil {
		return domain.Resource{}, err
	}
	if in.Labels != nil {
		var err error
		if labels, err = marshalLabels(in.Labels); err != nil {
			return domain.Resource{}, err
		}
	}
	var out domain.Resource
	err := s.pool.WithTx(ctx, org, func(ctx context.Context, tx pgx.Tx) error {
		q := db.New(tx)
		if _, err := authorize(ctx, q, a, org, domain.PermResourceWrite, project); err != nil {
			return err
		}
		if err := lockResourceParents(ctx, q, org, project); err != nil {
			return err
		}
		row, err := q.UpdateResource(ctx, db.UpdateResourceParams{OrganizationID: org, ProjectID: project, ID: id, Name: in.Name, Labels: labels})
		if err != nil {
			return notFound(err, "resource %s", id)
		}
		out, err = toResource(&row)
		return err
	})
	return out, conflictOn(err, resourceConflicts)
}

// DeleteResource soft-deletes a resource (resource.write).
func (s *Service) DeleteResource(ctx context.Context, a Actor, org, project, id uuid.UUID) error {
	return s.pool.WithTx(ctx, org, func(ctx context.Context, tx pgx.Tx) error {
		q := db.New(tx)
		if _, err := authorize(ctx, q, a, org, domain.PermResourceWrite, project); err != nil {
			return err
		}
		if err := lockResourceParents(ctx, q, org, project); err != nil {
			return err
		}
		n, err := q.SoftDeleteResource(ctx, db.SoftDeleteResourceParams{OrganizationID: org, ProjectID: project, ID: id})
		if err != nil {
			return fmt.Errorf("delete resource: %w", err)
		}
		if n == 0 {
			return domain.NotFoundf("resource %s", id)
		}
		return nil
	})
}

func lockResourceParents(ctx context.Context, q *db.Queries, org, project uuid.UUID) error {
	if _, err := q.LockLiveOrganization(ctx, org); err != nil {
		return notFound(err, "organization %s", org)
	}
	return lockLiveProject(ctx, q, org, project)
}

func marshalLabels(l map[string]string) ([]byte, error) {
	if l == nil {
		l = map[string]string{}
	}
	b, err := json.Marshal(l)
	if err != nil {
		return nil, fmt.Errorf("labels: %w", err)
	}
	return b, nil
}

func toResource(r *db.Resource) (domain.Resource, error) {
	labels := map[string]string{}
	if err := json.Unmarshal(r.Labels, &labels); err != nil {
		return domain.Resource{}, fmt.Errorf("resource %s labels: %w", r.ID, err)
	}
	return domain.Resource{ID: r.ID, OrganizationID: r.OrganizationID, ProjectID: r.ProjectID, Kind: domain.ResourceKind(r.Kind),
		Name: r.Name, Labels: labels, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}, nil
}
