package grpc

import (
	"context"
	"log/slog"
	"time"

	"google.golang.org/grpc"

	cpv1 "github.com/zodixdev/infrapilot/gen/go/infrapilot/controlplane/v1"
	"github.com/zodixdev/infrapilot/services/control-plane/internal/domain"
	"github.com/zodixdev/infrapilot/services/control-plane/internal/service"
)

// Server implements every control-plane gRPC service on top of service.Service.
type Server struct {
	cpv1.UnimplementedOrganizationServiceServer
	cpv1.UnimplementedProjectServiceServer
	cpv1.UnimplementedMemberServiceServer
	cpv1.UnimplementedResourceServiceServer
	cpv1.UnimplementedApiKeyServiceServer
	cpv1.UnimplementedAuditServiceServer
	cpv1.UnimplementedAuthorizationServiceServer

	svc *service.Service
	log *slog.Logger
}

// NewServer returns the gRPC adapter.
func NewServer(svc *service.Service, logger *slog.Logger) *Server {
	return &Server{svc: svc, log: logger}
}

// Register registers every service on r.
func (s *Server) Register(r grpc.ServiceRegistrar) {
	cpv1.RegisterOrganizationServiceServer(r, s)
	cpv1.RegisterProjectServiceServer(r, s)
	cpv1.RegisterMemberServiceServer(r, s)
	cpv1.RegisterResourceServiceServer(r, s)
	cpv1.RegisterApiKeyServiceServer(r, s)
	cpv1.RegisterAuditServiceServer(r, s)
	cpv1.RegisterAuthorizationServiceServer(r, s)
}

func (s *Server) fail(ctx context.Context, err error) error { return toStatus(ctx, s.log, err) }

// ---- organizations

// CreateOrganization implements the RPC of the same name.
func (s *Server) CreateOrganization(ctx context.Context, req *cpv1.CreateOrganizationRequest) (*cpv1.CreateOrganizationResponse, error) {
	o, err := s.svc.CreateOrganization(ctx, actorFrom(ctx), service.CreateOrganizationInput{Name: req.GetName()})
	if err != nil {
		return nil, s.fail(ctx, err)
	}
	return &cpv1.CreateOrganizationResponse{Organization: orgTo(&o)}, nil
}

// GetOrganization implements the RPC of the same name.
func (s *Server) GetOrganization(ctx context.Context, req *cpv1.GetOrganizationRequest) (*cpv1.GetOrganizationResponse, error) {
	var p ids
	org := p.parse("organization_id", req.GetOrganizationId())
	if err := p.err(); err != nil {
		return nil, s.fail(ctx, err)
	}
	o, err := s.svc.GetOrganization(ctx, actorFrom(ctx), org)
	if err != nil {
		return nil, s.fail(ctx, err)
	}
	return &cpv1.GetOrganizationResponse{Organization: orgTo(&o)}, nil
}

// ListMyOrganizations implements the RPC of the same name.
func (s *Server) ListMyOrganizations(ctx context.Context, req *cpv1.ListMyOrganizationsRequest) (*cpv1.ListMyOrganizationsResponse, error) {
	list, next, err := s.svc.ListMyOrganizations(ctx, actorFrom(ctx), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, s.fail(ctx, err)
	}
	out := &cpv1.ListMyOrganizationsResponse{NextPageToken: next}
	for i := range list {
		out.Organizations = append(out.Organizations, &cpv1.OrganizationWithRole{Organization: orgTo(&list[i].Organization), Role: roleToProto[list[i].Role]})
	}
	return out, nil
}

// UpdateOrganization implements the RPC of the same name.
func (s *Server) UpdateOrganization(ctx context.Context, req *cpv1.UpdateOrganizationRequest) (*cpv1.UpdateOrganizationResponse, error) {
	var p ids
	org := p.parse("organization_id", req.GetOrganizationId())
	if err := p.err(); err != nil {
		return nil, s.fail(ctx, err)
	}
	o, err := s.svc.UpdateOrganization(ctx, actorFrom(ctx), org, service.UpdateOrganizationInput{Name: req.Name})
	if err != nil {
		return nil, s.fail(ctx, err)
	}
	return &cpv1.UpdateOrganizationResponse{Organization: orgTo(&o)}, nil
}

// DeleteOrganization implements the RPC of the same name.
func (s *Server) DeleteOrganization(ctx context.Context, req *cpv1.DeleteOrganizationRequest) (*cpv1.DeleteOrganizationResponse, error) {
	var p ids
	org := p.parse("organization_id", req.GetOrganizationId())
	if err := p.err(); err != nil {
		return nil, s.fail(ctx, err)
	}
	if err := s.svc.DeleteOrganization(ctx, actorFrom(ctx), org); err != nil {
		return nil, s.fail(ctx, err)
	}
	return &cpv1.DeleteOrganizationResponse{}, nil
}

// ---- projects

// CreateProject implements the RPC of the same name.
func (s *Server) CreateProject(ctx context.Context, req *cpv1.CreateProjectRequest) (*cpv1.CreateProjectResponse, error) {
	var p ids
	org := p.parse("organization_id", req.GetOrganizationId())
	if err := p.err(); err != nil {
		return nil, s.fail(ctx, err)
	}
	pr, err := s.svc.CreateProject(ctx, actorFrom(ctx), org, service.CreateProjectInput{Name: req.GetName(), Slug: req.GetSlug(), Description: req.GetDescription()})
	if err != nil {
		return nil, s.fail(ctx, err)
	}
	return &cpv1.CreateProjectResponse{Project: projectTo(&pr)}, nil
}

// GetProject implements the RPC of the same name.
func (s *Server) GetProject(ctx context.Context, req *cpv1.GetProjectRequest) (*cpv1.GetProjectResponse, error) {
	var p ids
	org, id := p.parse("organization_id", req.GetOrganizationId()), p.parse("project_id", req.GetProjectId())
	if err := p.err(); err != nil {
		return nil, s.fail(ctx, err)
	}
	pr, err := s.svc.GetProject(ctx, actorFrom(ctx), org, id)
	if err != nil {
		return nil, s.fail(ctx, err)
	}
	return &cpv1.GetProjectResponse{Project: projectTo(&pr)}, nil
}

// ListProjects implements the RPC of the same name.
func (s *Server) ListProjects(ctx context.Context, req *cpv1.ListProjectsRequest) (*cpv1.ListProjectsResponse, error) {
	var p ids
	org := p.parse("organization_id", req.GetOrganizationId())
	if err := p.err(); err != nil {
		return nil, s.fail(ctx, err)
	}
	list, next, err := s.svc.ListProjects(ctx, actorFrom(ctx), org, req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, s.fail(ctx, err)
	}
	out := &cpv1.ListProjectsResponse{NextPageToken: next}
	for i := range list {
		out.Projects = append(out.Projects, projectTo(&list[i]))
	}
	return out, nil
}

// UpdateProject implements the RPC of the same name.
func (s *Server) UpdateProject(ctx context.Context, req *cpv1.UpdateProjectRequest) (*cpv1.UpdateProjectResponse, error) {
	var p ids
	org, id := p.parse("organization_id", req.GetOrganizationId()), p.parse("project_id", req.GetProjectId())
	if err := p.err(); err != nil {
		return nil, s.fail(ctx, err)
	}
	pr, err := s.svc.UpdateProject(ctx, actorFrom(ctx), org, id, service.UpdateProjectInput{Name: req.Name, Slug: req.Slug, Description: req.Description})
	if err != nil {
		return nil, s.fail(ctx, err)
	}
	return &cpv1.UpdateProjectResponse{Project: projectTo(&pr)}, nil
}

// DeleteProject implements the RPC of the same name.
func (s *Server) DeleteProject(ctx context.Context, req *cpv1.DeleteProjectRequest) (*cpv1.DeleteProjectResponse, error) {
	var p ids
	org, id := p.parse("organization_id", req.GetOrganizationId()), p.parse("project_id", req.GetProjectId())
	if err := p.err(); err != nil {
		return nil, s.fail(ctx, err)
	}
	if err := s.svc.DeleteProject(ctx, actorFrom(ctx), org, id); err != nil {
		return nil, s.fail(ctx, err)
	}
	return &cpv1.DeleteProjectResponse{}, nil
}

// ---- members

// AddMember implements the RPC of the same name.
func (s *Server) AddMember(ctx context.Context, req *cpv1.AddMemberRequest) (*cpv1.AddMemberResponse, error) {
	var p ids
	org := p.parse("organization_id", req.GetOrganizationId())
	if err := p.err(); err != nil {
		return nil, s.fail(ctx, err)
	}
	m, err := s.svc.AddMember(ctx, actorFrom(ctx), org, req.GetEmail(), roleFrom(req.GetRole()))
	if err != nil {
		return nil, s.fail(ctx, err)
	}
	return &cpv1.AddMemberResponse{Member: memberTo(&m)}, nil
}

// GetMember implements the RPC of the same name.
func (s *Server) GetMember(ctx context.Context, req *cpv1.GetMemberRequest) (*cpv1.GetMemberResponse, error) {
	var p ids
	org, user := p.parse("organization_id", req.GetOrganizationId()), p.parse("user_id", req.GetUserId())
	if err := p.err(); err != nil {
		return nil, s.fail(ctx, err)
	}
	m, err := s.svc.GetMember(ctx, actorFrom(ctx), org, user)
	if err != nil {
		return nil, s.fail(ctx, err)
	}
	return &cpv1.GetMemberResponse{Member: memberTo(&m)}, nil
}

// ListMembers implements the RPC of the same name.
func (s *Server) ListMembers(ctx context.Context, req *cpv1.ListMembersRequest) (*cpv1.ListMembersResponse, error) {
	var p ids
	org := p.parse("organization_id", req.GetOrganizationId())
	if err := p.err(); err != nil {
		return nil, s.fail(ctx, err)
	}
	list, next, err := s.svc.ListMembers(ctx, actorFrom(ctx), org, req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, s.fail(ctx, err)
	}
	out := &cpv1.ListMembersResponse{NextPageToken: next}
	for i := range list {
		out.Members = append(out.Members, memberTo(&list[i]))
	}
	return out, nil
}

// UpdateMemberRole implements the RPC of the same name.
func (s *Server) UpdateMemberRole(ctx context.Context, req *cpv1.UpdateMemberRoleRequest) (*cpv1.UpdateMemberRoleResponse, error) {
	var p ids
	org, user := p.parse("organization_id", req.GetOrganizationId()), p.parse("user_id", req.GetUserId())
	if err := p.err(); err != nil {
		return nil, s.fail(ctx, err)
	}
	m, err := s.svc.UpdateMemberRole(ctx, actorFrom(ctx), org, user, roleFrom(req.GetRole()))
	if err != nil {
		return nil, s.fail(ctx, err)
	}
	return &cpv1.UpdateMemberRoleResponse{Member: memberTo(&m)}, nil
}

// RemoveMember implements the RPC of the same name.
func (s *Server) RemoveMember(ctx context.Context, req *cpv1.RemoveMemberRequest) (*cpv1.RemoveMemberResponse, error) {
	var p ids
	org, user := p.parse("organization_id", req.GetOrganizationId()), p.parse("user_id", req.GetUserId())
	if err := p.err(); err != nil {
		return nil, s.fail(ctx, err)
	}
	if err := s.svc.RemoveMember(ctx, actorFrom(ctx), org, user); err != nil {
		return nil, s.fail(ctx, err)
	}
	return &cpv1.RemoveMemberResponse{}, nil
}

// ---- resources

// CreateResource implements the RPC of the same name.
func (s *Server) CreateResource(ctx context.Context, req *cpv1.CreateResourceRequest) (*cpv1.CreateResourceResponse, error) {
	var p ids
	org, proj := p.parse("organization_id", req.GetOrganizationId()), p.parse("project_id", req.GetProjectId())
	if err := p.err(); err != nil {
		return nil, s.fail(ctx, err)
	}
	r, err := s.svc.CreateResource(ctx, actorFrom(ctx), org, proj, service.CreateResourceInput{Kind: kindFrom(req.GetKind()), Name: req.GetName(), Labels: req.GetLabels()})
	if err != nil {
		return nil, s.fail(ctx, err)
	}
	return &cpv1.CreateResourceResponse{Resource: resourceTo(&r)}, nil
}

// GetResource implements the RPC of the same name.
func (s *Server) GetResource(ctx context.Context, req *cpv1.GetResourceRequest) (*cpv1.GetResourceResponse, error) {
	var p ids
	org, proj, id := p.parse("organization_id", req.GetOrganizationId()), p.parse("project_id", req.GetProjectId()), p.parse("resource_id", req.GetResourceId())
	if err := p.err(); err != nil {
		return nil, s.fail(ctx, err)
	}
	r, err := s.svc.GetResource(ctx, actorFrom(ctx), org, proj, id)
	if err != nil {
		return nil, s.fail(ctx, err)
	}
	return &cpv1.GetResourceResponse{Resource: resourceTo(&r)}, nil
}

// ListResources implements the RPC of the same name.
func (s *Server) ListResources(ctx context.Context, req *cpv1.ListResourcesRequest) (*cpv1.ListResourcesResponse, error) {
	var p ids
	org, proj := p.parse("organization_id", req.GetOrganizationId()), p.parse("project_id", req.GetProjectId())
	if err := p.err(); err != nil {
		return nil, s.fail(ctx, err)
	}
	var kind domain.ResourceKind
	if req.GetKind() != cpv1.ResourceKind_RESOURCE_KIND_UNSPECIFIED {
		if kind = kindFrom(req.GetKind()); kind == "" {
			kind = domain.ResourceKind(req.GetKind().String()) // reported as invalid by the service
		}
	}
	list, next, err := s.svc.ListResources(ctx, actorFrom(ctx), org, proj, kind, req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, s.fail(ctx, err)
	}
	out := &cpv1.ListResourcesResponse{NextPageToken: next}
	for i := range list {
		out.Resources = append(out.Resources, resourceTo(&list[i]))
	}
	return out, nil
}

// UpdateResource implements the RPC of the same name.
func (s *Server) UpdateResource(ctx context.Context, req *cpv1.UpdateResourceRequest) (*cpv1.UpdateResourceResponse, error) {
	var p ids
	org, proj, id := p.parse("organization_id", req.GetOrganizationId()), p.parse("project_id", req.GetProjectId()), p.parse("resource_id", req.GetResourceId())
	if err := p.err(); err != nil {
		return nil, s.fail(ctx, err)
	}
	in := service.UpdateResourceInput{Name: req.Name}
	if req.GetLabels() != nil {
		in.Labels = req.GetLabels().GetValues()
		if in.Labels == nil {
			in.Labels = map[string]string{} // present but empty: clear all labels
		}
	}
	r, err := s.svc.UpdateResource(ctx, actorFrom(ctx), org, proj, id, in)
	if err != nil {
		return nil, s.fail(ctx, err)
	}
	return &cpv1.UpdateResourceResponse{Resource: resourceTo(&r)}, nil
}

// DeleteResource implements the RPC of the same name.
func (s *Server) DeleteResource(ctx context.Context, req *cpv1.DeleteResourceRequest) (*cpv1.DeleteResourceResponse, error) {
	var p ids
	org, proj, id := p.parse("organization_id", req.GetOrganizationId()), p.parse("project_id", req.GetProjectId()), p.parse("resource_id", req.GetResourceId())
	if err := p.err(); err != nil {
		return nil, s.fail(ctx, err)
	}
	if err := s.svc.DeleteResource(ctx, actorFrom(ctx), org, proj, id); err != nil {
		return nil, s.fail(ctx, err)
	}
	return &cpv1.DeleteResourceResponse{}, nil
}

// ---- API keys

// CreateApiKey implements the RPC of the same name.
func (s *Server) CreateApiKey(ctx context.Context, req *cpv1.CreateApiKeyRequest) (*cpv1.CreateApiKeyResponse, error) { //nolint:revive // name fixed by the generated gRPC interface
	var p ids
	org, proj := p.parse("organization_id", req.GetOrganizationId()), p.parse("project_id", req.GetProjectId())
	if err := p.err(); err != nil {
		return nil, s.fail(ctx, err)
	}
	var exp *time.Time
	if req.GetExpireTime() != nil {
		t := req.GetExpireTime().AsTime()
		exp = &t
	}
	k, err := s.svc.CreateAPIKey(ctx, actorFrom(ctx), org, service.CreateAPIKeyInput{ProjectID: proj, Name: req.GetName(),
		Scopes: scopesFrom(req.GetScopes()), ExpiresAt: exp})
	if err != nil {
		return nil, s.fail(ctx, err)
	}
	return &cpv1.CreateApiKeyResponse{ApiKey: apiKeyTo(&k.Key), Secret: k.Secret}, nil
}

// GetApiKey implements the RPC of the same name.
func (s *Server) GetApiKey(ctx context.Context, req *cpv1.GetApiKeyRequest) (*cpv1.GetApiKeyResponse, error) { //nolint:revive // name fixed by the generated gRPC interface
	var p ids
	org, id := p.parse("organization_id", req.GetOrganizationId()), p.parse("api_key_id", req.GetApiKeyId())
	if err := p.err(); err != nil {
		return nil, s.fail(ctx, err)
	}
	k, err := s.svc.GetAPIKey(ctx, actorFrom(ctx), org, id)
	if err != nil {
		return nil, s.fail(ctx, err)
	}
	return &cpv1.GetApiKeyResponse{ApiKey: apiKeyTo(&k)}, nil
}

// ListApiKeys implements the RPC of the same name.
func (s *Server) ListApiKeys(ctx context.Context, req *cpv1.ListApiKeysRequest) (*cpv1.ListApiKeysResponse, error) { //nolint:revive // name fixed by the generated gRPC interface
	var p ids
	org, proj := p.parse("organization_id", req.GetOrganizationId()), p.optional("project_id", req.GetProjectId())
	if err := p.err(); err != nil {
		return nil, s.fail(ctx, err)
	}
	list, next, err := s.svc.ListAPIKeys(ctx, actorFrom(ctx), org, proj, req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, s.fail(ctx, err)
	}
	out := &cpv1.ListApiKeysResponse{NextPageToken: next}
	for i := range list {
		out.ApiKeys = append(out.ApiKeys, apiKeyTo(&list[i]))
	}
	return out, nil
}

// RevokeApiKey implements the RPC of the same name.
func (s *Server) RevokeApiKey(ctx context.Context, req *cpv1.RevokeApiKeyRequest) (*cpv1.RevokeApiKeyResponse, error) { //nolint:revive // name fixed by the generated gRPC interface
	var p ids
	org, id := p.parse("organization_id", req.GetOrganizationId()), p.parse("api_key_id", req.GetApiKeyId())
	if err := p.err(); err != nil {
		return nil, s.fail(ctx, err)
	}
	k, err := s.svc.RevokeAPIKey(ctx, actorFrom(ctx), org, id)
	if err != nil {
		return nil, s.fail(ctx, err)
	}
	return &cpv1.RevokeApiKeyResponse{ApiKey: apiKeyTo(&k)}, nil
}

// ---- audit

// ListAuditEvents implements the RPC of the same name.
func (s *Server) ListAuditEvents(ctx context.Context, req *cpv1.ListAuditEventsRequest) (*cpv1.ListAuditEventsResponse, error) {
	var p ids
	org := p.parse("organization_id", req.GetOrganizationId())
	if err := p.err(); err != nil {
		return nil, s.fail(ctx, err)
	}
	list, next, err := s.svc.ListAuditEvents(ctx, actorFrom(ctx), org, req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, s.fail(ctx, err)
	}
	out := &cpv1.ListAuditEventsResponse{NextPageToken: next}
	for i := range list {
		out.Events = append(out.Events, auditTo(&list[i]))
	}
	return out, nil
}

// ---- authorization

// CheckPermission implements the RPC of the same name.
func (s *Server) CheckPermission(ctx context.Context, req *cpv1.CheckPermissionRequest) (*cpv1.CheckPermissionResponse, error) {
	var p ids
	org, proj := p.parse("organization_id", req.GetOrganizationId()), p.optional("project_id", req.GetProjectId())
	if err := p.err(); err != nil {
		return nil, s.fail(ctx, err)
	}
	perm, ok := permFromProto[req.GetPermission()]
	if !ok {
		perm = domain.Permission(req.GetPermission().String()) // reported as invalid by the service
	}
	d, err := s.svc.CheckPermission(ctx, actorFrom(ctx), org, perm, proj)
	if err != nil {
		return nil, s.fail(ctx, err)
	}
	return &cpv1.CheckPermissionResponse{Allowed: d.Allowed, Role: roleToProto[d.Role]}, nil
}

// AuthenticateApiKey implements the RPC of the same name.
func (s *Server) AuthenticateApiKey(ctx context.Context, req *cpv1.AuthenticateApiKeyRequest) (*cpv1.AuthenticateApiKeyResponse, error) { //nolint:revive // name fixed by the generated gRPC interface
	id, err := s.svc.AuthenticateAPIKey(ctx, req.GetKeySha256())
	if err != nil {
		return nil, s.fail(ctx, err)
	}
	return &cpv1.AuthenticateApiKeyResponse{ApiKeyId: id.KeyID.String(), OrganizationId: id.OrganizationID.String(),
		ProjectId: id.ProjectID.String(), Scopes: scopesTo(id.Scopes), ExpireTime: tsPtr(id.ExpiresAt)}, nil
}
