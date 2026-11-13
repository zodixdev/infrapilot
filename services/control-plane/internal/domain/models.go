package domain

import (
	"time"

	"github.com/google/uuid"
)

// Organization is a tenant.
type Organization struct {
	ID        uuid.UUID
	Name      string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// OrganizationWithRole is an organization as seen by one of its members.
type OrganizationWithRole struct {
	Organization
	Role Role
}

// Project groups resources and API keys inside an organization.
type Project struct {
	ID             uuid.UUID
	OrganizationID uuid.UUID
	Name           string
	Slug           string
	Description    string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Member is a user's membership in an organization.
type Member struct {
	UserID         uuid.UUID
	OrganizationID uuid.UUID
	Email          string
	DisplayName    string
	Role           Role
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// ResourceKind classifies monitored resources.
type ResourceKind string

// Resource kinds (database CHECK in sync).
const (
	KindHost      ResourceKind = "host"
	KindService   ResourceKind = "service"
	KindEndpoint  ResourceKind = "endpoint"
	KindDatabase  ResourceKind = "database"
	KindQueue     ResourceKind = "queue"
	KindContainer ResourceKind = "container"
	KindOther     ResourceKind = "other"
)

// Valid reports whether k is a known kind.
func (k ResourceKind) Valid() bool {
	switch k {
	case KindHost, KindService, KindEndpoint, KindDatabase, KindQueue, KindContainer, KindOther:
		return true
	}
	return false
}

// Resource is a monitored object in a project's inventory.
type Resource struct {
	ID             uuid.UUID
	OrganizationID uuid.UUID
	ProjectID      uuid.UUID
	Kind           ResourceKind
	Name           string
	Labels         map[string]string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// APIKey is a project-scoped API key without its secret.
type APIKey struct {
	ID             uuid.UUID
	OrganizationID uuid.UUID
	ProjectID      uuid.UUID
	Name           string
	Prefix         string
	Scopes         []Scope
	CreatedBy      uuid.UUID
	CreatedAt      time.Time
	ExpiresAt      *time.Time
	RevokedAt      *time.Time
}

// ActorKind is who performed an action.
type ActorKind string

// Actor kinds (database CHECK in sync).
const (
	ActorUser   ActorKind = "user"
	ActorAPIKey ActorKind = "api_key"
)

// AuditEvent is one append-only audit record.
type AuditEvent struct {
	ID             uuid.UUID
	OrganizationID uuid.UUID
	ActorKind      ActorKind
	ActorID        uuid.UUID
	Action         string
	TargetType     string
	TargetID       string
	Details        map[string]string
	RequestID      string
	TraceID        string
	CreatedAt      time.Time
}

// KeyIdentity is what authenticating an API key yields (ADR-0022).
type KeyIdentity struct {
	KeyID          uuid.UUID
	OrganizationID uuid.UUID
	ProjectID      uuid.UUID
	Scopes         []Scope
	ExpiresAt      *time.Time
}
