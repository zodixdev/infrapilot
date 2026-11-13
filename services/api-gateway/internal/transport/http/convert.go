package http

import (
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	cpv1 "github.com/zodixdev/infrapilot/gen/go/infrapilot/controlplane/v1"
	"github.com/zodixdev/infrapilot/services/api-gateway/internal/transport/http/api"
)

// Mapping only: protobuf ↔ OpenAPI types. Ids come from control-plane and are valid UUIDs; a malformed one
// would be a contract violation and is reported as uuid.Nil (and caught by contract tests).

func id(s string) uuid.UUID {
	u, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil
	}
	return u
}

func optTime(t *timestamppb.Timestamp) *time.Time {
	if t == nil {
		return nil
	}
	v := t.AsTime()
	return &v
}

var (
	roleOut = map[cpv1.Role]api.Role{cpv1.Role_ROLE_OWNER: api.Owner, cpv1.Role_ROLE_ADMIN: api.Admin,
		cpv1.Role_ROLE_OPERATOR: api.Operator, cpv1.Role_ROLE_VIEWER: api.Viewer}
	roleIn = map[api.Role]cpv1.Role{api.Owner: cpv1.Role_ROLE_OWNER, api.Admin: cpv1.Role_ROLE_ADMIN,
		api.Operator: cpv1.Role_ROLE_OPERATOR, api.Viewer: cpv1.Role_ROLE_VIEWER}
	scopeOut = map[cpv1.ApiKeyScope]api.Scope{cpv1.ApiKeyScope_API_KEY_SCOPE_READ: api.Read,
		cpv1.ApiKeyScope_API_KEY_SCOPE_WRITE: api.Write, cpv1.ApiKeyScope_API_KEY_SCOPE_INGEST: api.Ingest}
	scopeIn = map[api.Scope]cpv1.ApiKeyScope{api.Read: cpv1.ApiKeyScope_API_KEY_SCOPE_READ,
		api.Write: cpv1.ApiKeyScope_API_KEY_SCOPE_WRITE, api.Ingest: cpv1.ApiKeyScope_API_KEY_SCOPE_INGEST}
	kindOut = map[cpv1.ResourceKind]api.ResourceKind{
		cpv1.ResourceKind_RESOURCE_KIND_HOST: api.Host, cpv1.ResourceKind_RESOURCE_KIND_SERVICE: api.Service,
		cpv1.ResourceKind_RESOURCE_KIND_ENDPOINT: api.Endpoint, cpv1.ResourceKind_RESOURCE_KIND_DATABASE: api.Database,
		cpv1.ResourceKind_RESOURCE_KIND_QUEUE: api.Queue, cpv1.ResourceKind_RESOURCE_KIND_CONTAINER: api.Container,
		cpv1.ResourceKind_RESOURCE_KIND_OTHER: api.Other,
	}
	kindIn = func() map[api.ResourceKind]cpv1.ResourceKind {
		m := map[api.ResourceKind]cpv1.ResourceKind{}
		for k, v := range kindOut {
			m[v] = k
		}
		return m
	}()
)

func page(next string) api.PageInfo {
	if next == "" {
		return api.PageInfo{HasMore: false}
	}
	return api.PageInfo{HasMore: true, NextCursor: &next}
}

func org(o *cpv1.Organization) api.Organization {
	return api.Organization{Id: id(o.GetId()), Name: o.GetName(), CreateTime: o.GetCreateTime().AsTime(), UpdateTime: o.GetUpdateTime().AsTime()}
}

func member(m *cpv1.Member) api.Member {
	return api.Member{UserId: id(m.GetUserId()), OrganizationId: id(m.GetOrganizationId()), Email: m.GetEmail(),
		DisplayName: m.GetDisplayName(), Role: roleOut[m.GetRole()], CreateTime: m.GetCreateTime().AsTime(), UpdateTime: m.GetUpdateTime().AsTime()}
}

func project(p *cpv1.Project) api.Project {
	return api.Project{Id: id(p.GetId()), OrganizationId: id(p.GetOrganizationId()), Name: p.GetName(), Slug: p.GetSlug(),
		Description: p.GetDescription(), CreateTime: p.GetCreateTime().AsTime(), UpdateTime: p.GetUpdateTime().AsTime()}
}

func resource(r *cpv1.Resource) api.Resource {
	labels := api.Labels(r.GetLabels())
	if labels == nil {
		labels = api.Labels{}
	}
	return api.Resource{Id: id(r.GetId()), OrganizationId: id(r.GetOrganizationId()), ProjectId: id(r.GetProjectId()),
		Kind: kindOut[r.GetKind()], Name: r.GetName(), Labels: labels, CreateTime: r.GetCreateTime().AsTime(), UpdateTime: r.GetUpdateTime().AsTime()}
}

func apiKey(k *cpv1.ApiKey) api.ApiKey {
	scopes := make([]api.Scope, 0, len(k.GetScopes()))
	for _, s := range k.GetScopes() {
		scopes = append(scopes, scopeOut[s])
	}
	return api.ApiKey{Id: id(k.GetId()), OrganizationId: id(k.GetOrganizationId()), ProjectId: id(k.GetProjectId()), Name: k.GetName(),
		Prefix: k.GetPrefix(), Scopes: scopes, CreateTime: k.GetCreateTime().AsTime(), ExpireTime: optTime(k.GetExpireTime()),
		RevokeTime: optTime(k.GetRevokeTime()), CreatedBy: id(k.GetCreatedBy())}
}

func auditEvent(e *cpv1.AuditEvent) api.AuditEvent {
	kind := api.AuditEventActorKindUser
	if e.GetActorKind() == cpv1.PrincipalKind_PRINCIPAL_KIND_API_KEY {
		kind = api.AuditEventActorKindApiKey
	}
	details := e.GetDetails()
	if details == nil {
		details = map[string]string{}
	}
	out := api.AuditEvent{Id: id(e.GetId()), OrganizationId: id(e.GetOrganizationId()), ActorKind: kind, ActorId: id(e.GetActorId()),
		Action: e.GetAction(), TargetType: e.GetTargetType(), TargetId: e.GetTargetId(), Details: details, CreateTime: e.GetCreateTime().AsTime()}
	if v := e.GetRequestId(); v != "" {
		out.RequestId = &v
	}
	if v := e.GetTraceId(); v != "" {
		out.TraceId = &v
	}
	return out
}

func pageArgs(limit *api.Limit, cursor *api.Cursor) (int32, string) {
	var l int32
	var c string
	if limit != nil {
		l = *limit
	}
	if cursor != nil {
		c = *cursor
	}
	return l, c
}
