package grpc

import (
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	cpv1 "github.com/zodixdev/infrapilot/gen/go/infrapilot/controlplane/v1"
	"github.com/zodixdev/infrapilot/services/control-plane/internal/domain"
)

var (
	roleToProto = map[domain.Role]cpv1.Role{
		domain.RoleOwner: cpv1.Role_ROLE_OWNER, domain.RoleAdmin: cpv1.Role_ROLE_ADMIN,
		domain.RoleOperator: cpv1.Role_ROLE_OPERATOR, domain.RoleViewer: cpv1.Role_ROLE_VIEWER,
	}
	permFromProto = map[cpv1.Permission]domain.Permission{
		cpv1.Permission_PERMISSION_ORG_READ: domain.PermOrgRead, cpv1.Permission_PERMISSION_ORG_UPDATE: domain.PermOrgUpdate,
		cpv1.Permission_PERMISSION_ORG_DELETE: domain.PermOrgDelete, cpv1.Permission_PERMISSION_MEMBER_READ: domain.PermMemberRead,
		cpv1.Permission_PERMISSION_MEMBER_MANAGE: domain.PermMemberManage, cpv1.Permission_PERMISSION_PROJECT_READ: domain.PermProjectRead,
		cpv1.Permission_PERMISSION_PROJECT_WRITE: domain.PermProjectWrite, cpv1.Permission_PERMISSION_RESOURCE_READ: domain.PermResourceRead,
		cpv1.Permission_PERMISSION_RESOURCE_WRITE: domain.PermResourceWrite, cpv1.Permission_PERMISSION_APIKEY_READ: domain.PermAPIKeyRead,
		cpv1.Permission_PERMISSION_APIKEY_MANAGE: domain.PermAPIKeyManage, cpv1.Permission_PERMISSION_AUDIT_READ: domain.PermAuditRead,
	}
	scopeToProto = map[domain.Scope]cpv1.ApiKeyScope{
		domain.ScopeRead: cpv1.ApiKeyScope_API_KEY_SCOPE_READ, domain.ScopeWrite: cpv1.ApiKeyScope_API_KEY_SCOPE_WRITE,
		domain.ScopeIngest: cpv1.ApiKeyScope_API_KEY_SCOPE_INGEST,
	}
	kindToProto = map[domain.ResourceKind]cpv1.ResourceKind{
		domain.KindHost: cpv1.ResourceKind_RESOURCE_KIND_HOST, domain.KindService: cpv1.ResourceKind_RESOURCE_KIND_SERVICE,
		domain.KindEndpoint: cpv1.ResourceKind_RESOURCE_KIND_ENDPOINT, domain.KindDatabase: cpv1.ResourceKind_RESOURCE_KIND_DATABASE,
		domain.KindQueue: cpv1.ResourceKind_RESOURCE_KIND_QUEUE, domain.KindContainer: cpv1.ResourceKind_RESOURCE_KIND_CONTAINER,
		domain.KindOther: cpv1.ResourceKind_RESOURCE_KIND_OTHER,
	}
)

func invert[K comparable, V comparable](m map[K]V) map[V]K {
	out := make(map[V]K, len(m))
	for k, v := range m {
		out[v] = k
	}
	return out
}

var (
	roleFromProto  = invert(roleToProto)
	scopeFromProto = invert(scopeToProto)
	kindFromProto  = invert(kindToProto)
)

// Unknown enum values map to the zero value; the domain validators then report them as invalid fields.
func roleFrom(r cpv1.Role) domain.Role                 { return roleFromProto[r] }
func kindFrom(k cpv1.ResourceKind) domain.ResourceKind { return kindFromProto[k] }

func scopesFrom(in []cpv1.ApiKeyScope) []domain.Scope {
	out := make([]domain.Scope, len(in))
	for i, s := range in {
		if d, ok := scopeFromProto[s]; ok {
			out[i] = d
		} else {
			out[i] = domain.Scope(s.String())
		}
	}
	return out
}

func scopesTo(in []domain.Scope) []cpv1.ApiKeyScope {
	out := make([]cpv1.ApiKeyScope, len(in))
	for i, s := range in {
		out[i] = scopeToProto[s]
	}
	return out
}

func ts(t time.Time) *timestamppb.Timestamp { return timestamppb.New(t) }

func tsPtr(t *time.Time) *timestamppb.Timestamp {
	if t == nil {
		return nil
	}
	return timestamppb.New(*t)
}

func orgTo(o *domain.Organization) *cpv1.Organization {
	return &cpv1.Organization{Id: o.ID.String(), Name: o.Name, CreateTime: ts(o.CreatedAt), UpdateTime: ts(o.UpdatedAt)}
}

func projectTo(p *domain.Project) *cpv1.Project {
	return &cpv1.Project{Id: p.ID.String(), OrganizationId: p.OrganizationID.String(), Name: p.Name, Slug: p.Slug,
		Description: p.Description, CreateTime: ts(p.CreatedAt), UpdateTime: ts(p.UpdatedAt)}
}

func memberTo(m *domain.Member) *cpv1.Member {
	return &cpv1.Member{UserId: m.UserID.String(), OrganizationId: m.OrganizationID.String(), Email: m.Email,
		DisplayName: m.DisplayName, Role: roleToProto[m.Role], CreateTime: ts(m.CreatedAt), UpdateTime: ts(m.UpdatedAt)}
}

func resourceTo(r *domain.Resource) *cpv1.Resource {
	return &cpv1.Resource{Id: r.ID.String(), OrganizationId: r.OrganizationID.String(), ProjectId: r.ProjectID.String(),
		Kind: kindToProto[r.Kind], Name: r.Name, Labels: r.Labels, CreateTime: ts(r.CreatedAt), UpdateTime: ts(r.UpdatedAt)}
}

func apiKeyTo(k *domain.APIKey) *cpv1.ApiKey {
	return &cpv1.ApiKey{Id: k.ID.String(), OrganizationId: k.OrganizationID.String(), ProjectId: k.ProjectID.String(),
		Name: k.Name, Prefix: k.Prefix, Scopes: scopesTo(k.Scopes), CreateTime: ts(k.CreatedAt),
		ExpireTime: tsPtr(k.ExpiresAt), RevokeTime: tsPtr(k.RevokedAt), CreatedBy: k.CreatedBy.String()}
}

func auditTo(e *domain.AuditEvent) *cpv1.AuditEvent {
	kind := cpv1.PrincipalKind_PRINCIPAL_KIND_USER
	if e.ActorKind == domain.ActorAPIKey {
		kind = cpv1.PrincipalKind_PRINCIPAL_KIND_API_KEY
	}
	return &cpv1.AuditEvent{Id: e.ID.String(), OrganizationId: e.OrganizationID.String(), ActorKind: kind,
		ActorId: e.ActorID.String(), Action: e.Action, TargetType: e.TargetType, TargetId: e.TargetID,
		Details: e.Details, RequestId: e.RequestID, TraceId: e.TraceID, CreateTime: ts(e.CreatedAt)}
}

// ids parses request UUIDs, collecting violations for every malformed one.
type ids struct{ v domain.Validator }

func (p *ids) parse(field, s string) uuid.UUID {
	id, err := uuid.Parse(s)
	if err != nil || id == uuid.Nil {
		p.v.Add(field, "must be a UUID")
		return uuid.Nil
	}
	return id
}

// optional parses an optional UUID ("" → uuid.Nil).
func (p *ids) optional(field, s string) uuid.UUID {
	if s == "" {
		return uuid.Nil
	}
	return p.parse(field, s)
}

func (p *ids) err() error { return p.v.Err() }
