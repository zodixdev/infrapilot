// Package service implements the control-plane use cases: every operation authorizes the actor inside the same
// transaction that reads or writes (ADR-0022), runs under the tenant context of pkg/postgres (ADR-0018), audits
// membership, key and organization changes, and deduplicates creates by Idempotency-Key (ADR-0024).
package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/otel/trace"

	"github.com/zodixdev/infrapilot/pkg/postgres"
	"github.com/zodixdev/infrapilot/services/control-plane/internal/domain"
	"github.com/zodixdev/infrapilot/services/control-plane/internal/store/db"
)

// Service implements the use cases. It is safe for concurrent use.
type Service struct {
	pool  *postgres.Pool
	log   *slog.Logger
	now   func() time.Time
	users *userCache
}

// Option configures the service.
type Option func(*Service)

// WithLogger sets the logger.
func WithLogger(l *slog.Logger) Option { return func(s *Service) { s.log = l } }

// WithClock replaces time.Now (tests).
func WithClock(now func() time.Time) Option { return func(s *Service) { s.now = now } }

// New returns a service over the runtime (RLS-enforced) pool.
func New(pool *postgres.Pool, opts ...Option) (*Service, error) {
	if pool.BypassesRLS() {
		return nil, errors.New("service: the request path must use the <svc>_app role (RLS enforced), not a BYPASSRLS role")
	}
	s := &Service{pool: pool, log: slog.New(slog.DiscardHandler), now: time.Now, users: newUserCache(10_000, 10*time.Minute)}
	for _, o := range opts {
		o(s)
	}
	return s, nil
}

// Principal is the authenticated caller as asserted by the edge (ADR-0025).
type Principal struct {
	Kind        domain.ActorKind
	Subject     string // users: OIDC subject
	Email       string
	DisplayName string
	APIKeyID    uuid.UUID // API keys
}

// Actor is a resolved principal: a user record or an API key id.
type Actor struct {
	Kind domain.ActorKind
	ID   uuid.UUID
}

type requestMetaKey struct{}

// RequestMeta carries request identifiers recorded in audit events.
type RequestMeta struct {
	RequestID      string
	IdempotencyKey string
}

// WithRequestMeta attaches request identifiers to ctx (transport layer).
func WithRequestMeta(ctx context.Context, m RequestMeta) context.Context {
	return context.WithValue(ctx, requestMetaKey{}, m)
}

func requestMeta(ctx context.Context) RequestMeta {
	m, _ := ctx.Value(requestMetaKey{}).(RequestMeta)
	return m
}

// ResolveActor maps a principal to an actor. Users are created on first sight and refreshed at most every
// 10 minutes per replica (e-mail and name come from the identity provider). API keys are only parsed here;
// every operation re-checks the key inside its tenant transaction.
func (s *Service) ResolveActor(ctx context.Context, p Principal) (Actor, error) {
	switch p.Kind {
	case domain.ActorAPIKey:
		if p.APIKeyID == uuid.Nil {
			return Actor{}, fmt.Errorf("api key principal without key id: %w", domain.ErrUnauthenticated)
		}
		return Actor{Kind: domain.ActorAPIKey, ID: p.APIKeyID}, nil
	case domain.ActorUser:
	default:
		return Actor{}, fmt.Errorf("principal kind %q: %w", p.Kind, domain.ErrUnauthenticated)
	}
	if p.Subject == "" || len(p.Subject) > 255 {
		return Actor{}, fmt.Errorf("user principal without a valid subject: %w", domain.ErrUnauthenticated)
	}
	if id, ok := s.users.get(p, s.now()); ok {
		return Actor{Kind: domain.ActorUser, ID: id}, nil
	}
	email, name := truncate(p.Email, domain.MaxEmailLen), truncate(p.DisplayName, domain.MaxNameLen)
	var id uuid.UUID
	err := s.pool.WithSystemTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		u, err := db.New(tx).UpsertUser(ctx, db.UpsertUserParams{ID: newID(), Subject: p.Subject, Email: email, DisplayName: name})
		id = u.ID
		return err
	})
	if err != nil {
		return Actor{}, fmt.Errorf("resolve user: %w", err)
	}
	s.users.put(p, id, s.now())
	return Actor{Kind: domain.ActorUser, ID: id}, nil
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

// grant is the outcome of authorize: the user's role (users) or the key's project (API keys).
type grant struct {
	role       domain.Role
	keyProject uuid.UUID
}

// authorize checks perm for actor in org (and project, for project-scoped permissions) inside tx. Non-members
// and keys of other organizations get NOT_FOUND so foreign ids reveal nothing (ADR-0022).
func authorize(ctx context.Context, q *db.Queries, a Actor, org uuid.UUID, perm domain.Permission, project uuid.UUID) (grant, error) {
	switch a.Kind {
	case domain.ActorUser:
		role, err := q.GetRole(ctx, db.GetRoleParams{OrganizationID: org, UserID: a.ID})
		if errors.Is(err, pgx.ErrNoRows) {
			return grant{}, domain.NotFoundf("organization %s", org)
		}
		if err != nil {
			return grant{}, fmt.Errorf("authorize: %w", err)
		}
		r := domain.Role(role)
		if !domain.RoleAllows(r, perm) {
			return grant{role: r}, domain.Deniedf("role %s lacks %s", r, perm)
		}
		return grant{role: r}, nil
	case domain.ActorAPIKey:
		row, err := q.GetActiveAPIKeyScopes(ctx, db.GetActiveAPIKeyScopesParams{OrganizationID: org, ID: a.ID})
		if errors.Is(err, pgx.ErrNoRows) {
			// Unknown in this org: a key of another tenant, or revoked/expired since the edge cached it.
			return grant{}, domain.NotFoundf("organization %s", org)
		}
		if err != nil {
			return grant{}, fmt.Errorf("authorize: %w", err)
		}
		scopes := toScopes(row.Scopes)
		proj := ""
		if project != uuid.Nil {
			proj = project.String()
		}
		if !domain.KeyAllows(scopes, row.ProjectID.String(), proj, perm) {
			return grant{keyProject: row.ProjectID}, domain.Deniedf("api key lacks %s here", perm)
		}
		return grant{keyProject: row.ProjectID}, nil
	}
	return grant{}, domain.ErrUnauthenticated
}

func toScopes(in []string) []domain.Scope {
	out := make([]domain.Scope, len(in))
	for i, s := range in {
		out[i] = domain.Scope(s)
	}
	return out
}

// audit appends an audit event in the caller's transaction.
func audit(ctx context.Context, q *db.Queries, org uuid.UUID, a Actor, action, targetType, targetID string, details map[string]string) error {
	if details == nil {
		details = map[string]string{}
	}
	b, err := json.Marshal(details)
	if err != nil {
		return fmt.Errorf("audit: %w", err)
	}
	traceID := ""
	if sc := trace.SpanContextFromContext(ctx); sc.HasTraceID() {
		traceID = sc.TraceID().String()
	}
	err = q.InsertAuditEvent(ctx, db.InsertAuditEventParams{
		ID: newID(), OrganizationID: org, ActorKind: string(a.Kind), ActorID: a.ID, Action: action,
		TargetType: targetType, TargetID: targetID, Details: b, RequestID: requestMeta(ctx).RequestID, TraceID: traceID,
	})
	if err != nil {
		return fmt.Errorf("audit %s: %w", action, err)
	}
	return nil
}

// idempotency describes the dedup record of one create (ADR-0024); nil when the request carries no key.
type idempotency struct {
	key  string
	op   string
	hash []byte
}

func newIdempotency(ctx context.Context, op string, input any) (*idempotency, error) {
	key := requestMeta(ctx).IdempotencyKey
	if key == "" {
		return nil, nil //nolint:nilnil // no key: the create is not deduplicated
	}
	if len(key) > 128 {
		v := domain.Validator{}
		v.Add("idempotency_key", "must be at most 128 characters")
		return nil, v.Err()
	}
	// encoding/json is deterministic for structs and sorts map keys: a stable fingerprint of the input.
	b, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("idempotency fingerprint: %w", err)
	}
	h := sha256.Sum256(append([]byte(op+"\x00"), b...))
	return &idempotency{key: key, op: op, hash: h[:]}, nil
}

// createIdempotent runs create inside WithTx(org). With an idempotency record already present it returns
// replay(resourceID) instead (or ErrIdempotencyKeyReused on a different request). A concurrent duplicate loses
// the unique-key race; the transaction is then retried once and takes the replay path.
func createIdempotent[T any](ctx context.Context, s *Service, org uuid.UUID, a Actor, idem *idempotency,
	create func(ctx context.Context, q *db.Queries) (T, uuid.UUID, error),
	replay func(ctx context.Context, q *db.Queries, id uuid.UUID) (T, error),
) (T, error) {
	var out T
	attempt := func() error {
		return s.pool.WithTx(ctx, org, func(ctx context.Context, tx pgx.Tx) error {
			q := db.New(tx)
			if idem != nil {
				rec, err := q.GetIdempotencyKey(ctx, db.GetIdempotencyKeyParams{OrganizationID: org, PrincipalID: a.ID, Key: idem.key})
				switch {
				case err == nil:
					if rec.Operation != idem.op || !bytes.Equal(rec.RequestHash, idem.hash) {
						return domain.ErrIdempotencyKeyReused
					}
					out, err = replay(ctx, q, rec.ResourceID)
					return err
				case !errors.Is(err, pgx.ErrNoRows):
					return fmt.Errorf("idempotency lookup: %w", err)
				}
			}
			v, id, err := create(ctx, q)
			if err != nil {
				return err
			}
			if idem != nil {
				if err := q.InsertIdempotencyKey(ctx, db.InsertIdempotencyKeyParams{
					OrganizationID: org, PrincipalID: a.ID, Key: idem.key, Operation: idem.op, RequestHash: idem.hash, ResourceID: id,
				}); err != nil {
					return fmt.Errorf("idempotency record: %w", err)
				}
			}
			out = v
			return nil
		})
	}
	err := attempt()
	if idem != nil && isUniqueViolation(err) {
		err = attempt()
	}
	return out, err
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func constraint(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.ConstraintName
	}
	return ""
}

// conflictOn maps unique violations of known constraints to domain conflicts with a caller-safe message.
func conflictOn(err error, messages map[string]string) error {
	if isUniqueViolation(err) {
		if msg, ok := messages[constraint(err)]; ok {
			return domain.Conflictf("%s", msg)
		}
	}
	return err
}

func newID() uuid.UUID { return uuid.Must(uuid.NewV7()) }

// userCache maps OIDC subjects to user ids. Entries expire so e-mail/name changes and last-seen reach the
// database; the subject → id mapping itself never changes.
type userCache struct {
	mu      sync.Mutex
	max     int
	ttl     time.Duration
	entries map[string]userEntry
}

type userEntry struct {
	id          uuid.UUID
	email, name string
	at          time.Time
}

func newUserCache(maxEntries int, ttl time.Duration) *userCache {
	return &userCache{max: maxEntries, ttl: ttl, entries: map[string]userEntry{}}
}

func (c *userCache) get(p Principal, now time.Time) (uuid.UUID, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[p.Subject]
	if !ok || now.Sub(e.at) > c.ttl || e.email != p.Email || e.name != p.DisplayName {
		return uuid.Nil, false
	}
	return e.id, true
}

func (c *userCache) put(p Principal, id uuid.UUID, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= c.max {
		clear(c.entries) // bounded memory; refills from the database at one upsert per user
	}
	c.entries[p.Subject] = userEntry{id: id, email: p.Email, name: p.DisplayName, at: now}
}
