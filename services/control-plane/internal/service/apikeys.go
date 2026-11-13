package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/zodixdev/infrapilot/pkg/postgres"
	"github.com/zodixdev/infrapilot/services/control-plane/internal/domain"
	"github.com/zodixdev/infrapilot/services/control-plane/internal/store/db"
)

// CreateAPIKeyInput is the request of CreateAPIKey.
type CreateAPIKeyInput struct {
	ProjectID uuid.UUID      `json:"project_id"`
	Name      string         `json:"name"`
	Scopes    []domain.Scope `json:"scopes"`
	ExpiresAt *time.Time     `json:"expires_at"`
}

// CreatedAPIKey is a new key with its secret, shown exactly once (empty on idempotent replays).
type CreatedAPIKey struct {
	Key    domain.APIKey
	Secret string
}

// CreateAPIKey creates a project API key (apikey.manage). Only SHA-256 of the secret is stored (ADR-0022).
func (s *Service) CreateAPIKey(ctx context.Context, a Actor, org uuid.UUID, in CreateAPIKeyInput) (CreatedAPIKey, error) {
	var v domain.Validator
	domain.CheckName(&v, "name", in.Name)
	domain.CheckScopes(&v, "scopes", in.Scopes)
	domain.CheckExpiry(&v, "expire_time", in.ExpiresAt, s.now())
	v.Check(in.ProjectID != uuid.Nil, "project_id", "is required")
	if err := v.Err(); err != nil {
		return CreatedAPIKey{}, err
	}
	idem, idemErr := newIdempotency(ctx, "CreateAPIKey", in)
	if idemErr != nil {
		return CreatedAPIKey{}, idemErr
	}
	return createIdempotent(ctx, s, org, a, idem,
		func(ctx context.Context, q *db.Queries) (CreatedAPIKey, uuid.UUID, error) {
			if a.Kind != domain.ActorUser {
				if _, err := authorize(ctx, q, a, org, domain.PermOrgRead, uuid.Nil); err != nil {
					return CreatedAPIKey{}, uuid.Nil, err
				}
				return CreatedAPIKey{}, uuid.Nil, domain.Deniedf("api keys cannot create api keys")
			}
			if _, err := authorize(ctx, q, a, org, domain.PermAPIKeyManage, uuid.Nil); err != nil {
				return CreatedAPIKey{}, uuid.Nil, err
			}
			if err := lockLiveProject(ctx, q, org, in.ProjectID); err != nil {
				return CreatedAPIKey{}, uuid.Nil, err
			}
			secret, hash, prefix, err := domain.NewAPIKey()
			if err != nil {
				return CreatedAPIKey{}, uuid.Nil, err
			}
			scopes := make([]string, len(in.Scopes))
			for i, sc := range in.Scopes {
				scopes[i] = string(sc)
			}
			row, err := q.InsertAPIKey(ctx, db.InsertAPIKeyParams{ID: newID(), OrganizationID: org, ProjectID: in.ProjectID,
				Name: in.Name, Prefix: prefix, Scopes: scopes, CreatedBy: a.ID, ExpiresAt: in.ExpiresAt})
			if err != nil {
				return CreatedAPIKey{}, uuid.Nil, fmt.Errorf("create api key: %w", err)
			}
			if err := q.InsertAPIKeyIndex(ctx, db.InsertAPIKeyIndexParams{KeyHash: hash[:], KeyID: row.ID, OrganizationID: org,
				ProjectID: in.ProjectID, Scopes: scopes, ExpiresAt: in.ExpiresAt}); err != nil {
				return CreatedAPIKey{}, uuid.Nil, fmt.Errorf("create api key index: %w", err)
			}
			if err := audit(ctx, q, org, a, "apikey.created", "api_key", row.ID.String(), map[string]string{
				"name": in.Name, "project_id": in.ProjectID.String(), "prefix": prefix,
			}); err != nil {
				return CreatedAPIKey{}, uuid.Nil, err
			}
			return CreatedAPIKey{Key: toAPIKey(&row), Secret: secret}, row.ID, nil
		},
		func(ctx context.Context, q *db.Queries, id uuid.UUID) (CreatedAPIKey, error) {
			k, err := getAPIKey(ctx, q, a, org, id)
			return CreatedAPIKey{Key: k}, err // the secret is never stored, so a replay cannot return it
		})
}

// GetAPIKey returns key metadata (apikey.read).
func (s *Service) GetAPIKey(ctx context.Context, a Actor, org, id uuid.UUID) (domain.APIKey, error) {
	var out domain.APIKey
	err := s.pool.WithTx(ctx, org, func(ctx context.Context, tx pgx.Tx) (err error) {
		out, err = getAPIKey(ctx, db.New(tx), a, org, id)
		return err
	}, postgres.ReadOnly())
	return out, err
}

func getAPIKey(ctx context.Context, q *db.Queries, a Actor, org, id uuid.UUID) (domain.APIKey, error) {
	if _, err := authorize(ctx, q, a, org, domain.PermAPIKeyRead, uuid.Nil); err != nil {
		return domain.APIKey{}, err
	}
	row, err := q.GetAPIKey(ctx, db.GetAPIKeyParams{OrganizationID: org, ID: id})
	if err != nil {
		return domain.APIKey{}, notFound(err, "api key %s", id)
	}
	return toAPIKey(&row), nil
}

// ListAPIKeys lists the organization's keys, optionally of one project (apikey.read).
func (s *Service) ListAPIKeys(ctx context.Context, a Actor, org, project uuid.UUID, size int32, token string) ([]domain.APIKey, string, error) {
	var v domain.Validator
	page := domain.ParsePage(&v, size, token)
	if err := v.Err(); err != nil {
		return nil, "", err
	}
	var rows []db.ApiKey
	err := s.pool.WithTx(ctx, org, func(ctx context.Context, tx pgx.Tx) error {
		q := db.New(tx)
		if _, err := authorize(ctx, q, a, org, domain.PermAPIKeyRead, uuid.Nil); err != nil {
			return err
		}
		var err error
		if project == uuid.Nil {
			rows, err = q.ListAPIKeys(ctx, db.ListAPIKeysParams{OrganizationID: org, After: page.Cursor, Lim: page.Limit()})
		} else {
			rows, err = q.ListProjectAPIKeys(ctx, db.ListProjectAPIKeysParams{OrganizationID: org, ProjectID: project, After: page.Cursor, Lim: page.Limit()})
		}
		return err
	}, postgres.ReadOnly())
	if err != nil {
		return nil, "", err
	}
	rows, more := domain.Trim(page, rows)
	out := make([]domain.APIKey, len(rows))
	var last uuid.UUID
	for i := range rows {
		r := &rows[i]
		out[i], last = toAPIKey(r), r.ID
	}
	return out, domain.NextToken(more, last), nil
}

// RevokeAPIKey revokes a key (apikey.manage). Revoking twice is a no-op that returns the key.
func (s *Service) RevokeAPIKey(ctx context.Context, a Actor, org, id uuid.UUID) (domain.APIKey, error) {
	var out domain.APIKey
	err := s.pool.WithTx(ctx, org, func(ctx context.Context, tx pgx.Tx) error {
		q := db.New(tx)
		if _, err := authorize(ctx, q, a, org, domain.PermAPIKeyManage, uuid.Nil); err != nil {
			return err
		}
		before, err := q.GetAPIKey(ctx, db.GetAPIKeyParams{OrganizationID: org, ID: id})
		if err != nil {
			return notFound(err, "api key %s", id)
		}
		row, err := q.RevokeAPIKey(ctx, db.RevokeAPIKeyParams{OrganizationID: org, ID: id})
		if err != nil {
			return fmt.Errorf("revoke api key: %w", err)
		}
		out = toAPIKey(&row)
		if before.RevokedAt != nil {
			return nil
		}
		return audit(ctx, q, org, a, "apikey.revoked", "api_key", id.String(), map[string]string{"prefix": row.Prefix})
	})
	return out, err
}

// AuthenticateAPIKey resolves a key by the SHA-256 of its text (pre-tenant lookup, ADR-0018).
func (s *Service) AuthenticateAPIKey(ctx context.Context, hash []byte) (domain.KeyIdentity, error) {
	if len(hash) != 32 {
		return domain.KeyIdentity{}, domain.ErrUnauthenticated
	}
	var out domain.KeyIdentity
	err := s.pool.WithSystemTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		row, err := db.New(tx).LookupAPIKey(ctx, hash)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrUnauthenticated
		}
		if err != nil {
			return fmt.Errorf("authenticate api key: %w", err)
		}
		out = domain.KeyIdentity{KeyID: row.KeyID, OrganizationID: row.OrganizationID, ProjectID: row.ProjectID,
			Scopes: toScopes(row.Scopes), ExpiresAt: row.ExpiresAt}
		return nil
	}, postgres.ReadOnly())
	return out, err
}

func toAPIKey(r *db.ApiKey) domain.APIKey {
	return domain.APIKey{ID: r.ID, OrganizationID: r.OrganizationID, ProjectID: r.ProjectID, Name: r.Name, Prefix: r.Prefix,
		Scopes: toScopes(r.Scopes), CreatedBy: r.CreatedBy, CreatedAt: r.CreatedAt, ExpiresAt: r.ExpiresAt, RevokedAt: r.RevokedAt}
}
