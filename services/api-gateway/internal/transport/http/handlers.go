package http

import (
	"context"

	"google.golang.org/protobuf/types/known/timestamppb"

	cpv1 "github.com/zodixdev/infrapilot/gen/go/infrapilot/controlplane/v1"
	"github.com/zodixdev/infrapilot/services/api-gateway/internal/principal"
	"github.com/zodixdev/infrapilot/services/api-gateway/internal/transport/http/api"
)

// Upstreams are the control-plane clients the handlers call.
type Upstreams struct {
	Organizations cpv1.OrganizationServiceClient
	Projects      cpv1.ProjectServiceClient
	Members       cpv1.MemberServiceClient
	Resources     cpv1.ResourceServiceClient
	APIKeys       cpv1.ApiKeyServiceClient
	Audit         cpv1.AuditServiceClient
}

// Handlers implements the generated strict server by forwarding to control-plane (ADR-0002: the gateway holds
// no business logic). Errors returned here are mapped to problem+json by the response error handler.
type Handlers struct {
	up Upstreams
}

var _ api.StrictServerInterface = (*Handlers)(nil)

// NewHandlers returns the API implementation.
func NewHandlers(up Upstreams) *Handlers { return &Handlers{up: up} }

func out(ctx context.Context) (context.Context, error) { return principal.Outgoing(ctx) }

func withIdem(ctx context.Context, key *api.IdempotencyKey) context.Context {
	if key == nil {
		return ctx
	}
	return principal.WithIdempotencyKey(ctx, *key)
}

// GetHealth reports the gateway alive; downstream health is on the admin port's /readyz.
func (h *Handlers) GetHealth(context.Context, api.GetHealthRequestObject) (api.GetHealthResponseObject, error) {
	return api.GetHealth200JSONResponse{Status: "ok"}, nil
}

// ---- organizations

// ListMyOrganizations implements the operation of the same name.
func (h *Handlers) ListMyOrganizations(ctx context.Context, r api.ListMyOrganizationsRequestObject) (api.ListMyOrganizationsResponseObject, error) {
	ctx, err := out(ctx)
	if err != nil {
		return nil, err
	}
	size, token := pageArgs(r.Params.Limit, r.Params.Cursor)
	resp, err := h.up.Organizations.ListMyOrganizations(ctx, &cpv1.ListMyOrganizationsRequest{PageSize: size, PageToken: token})
	if err != nil {
		return nil, err
	}
	body := api.OrganizationMembershipList{Items: make([]api.OrganizationMembership, 0, len(resp.GetOrganizations())), Page: page(resp.GetNextPageToken())}
	for _, o := range resp.GetOrganizations() {
		body.Items = append(body.Items, api.OrganizationMembership{Organization: org(o.GetOrganization()), Role: roleOut[o.GetRole()]})
	}
	return api.ListMyOrganizations200JSONResponse{Body: body}, nil
}

// CreateOrganization implements the operation of the same name.
func (h *Handlers) CreateOrganization(ctx context.Context, r api.CreateOrganizationRequestObject) (api.CreateOrganizationResponseObject, error) {
	ctx, err := out(withIdem(ctx, r.Params.IdempotencyKey))
	if err != nil {
		return nil, err
	}
	resp, err := h.up.Organizations.CreateOrganization(ctx, &cpv1.CreateOrganizationRequest{Name: r.Body.Name})
	if err != nil {
		return nil, err
	}
	return api.CreateOrganization201JSONResponse{Body: org(resp.GetOrganization())}, nil
}

// GetOrganization implements the operation of the same name.
func (h *Handlers) GetOrganization(ctx context.Context, r api.GetOrganizationRequestObject) (api.GetOrganizationResponseObject, error) {
	ctx, err := out(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := h.up.Organizations.GetOrganization(ctx, &cpv1.GetOrganizationRequest{OrganizationId: r.OrgId.String()})
	if err != nil {
		return nil, err
	}
	return api.GetOrganization200JSONResponse{Body: org(resp.GetOrganization())}, nil
}

// UpdateOrganization implements the operation of the same name.
func (h *Handlers) UpdateOrganization(ctx context.Context, r api.UpdateOrganizationRequestObject) (api.UpdateOrganizationResponseObject, error) {
	ctx, err := out(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := h.up.Organizations.UpdateOrganization(ctx, &cpv1.UpdateOrganizationRequest{OrganizationId: r.OrgId.String(), Name: r.Body.Name})
	if err != nil {
		return nil, err
	}
	return api.UpdateOrganization200JSONResponse{Body: org(resp.GetOrganization())}, nil
}

// DeleteOrganization implements the operation of the same name.
func (h *Handlers) DeleteOrganization(ctx context.Context, r api.DeleteOrganizationRequestObject) (api.DeleteOrganizationResponseObject, error) {
	ctx, err := out(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := h.up.Organizations.DeleteOrganization(ctx, &cpv1.DeleteOrganizationRequest{OrganizationId: r.OrgId.String()}); err != nil {
		return nil, err
	}
	return api.DeleteOrganization204Response{}, nil
}

// ---- members

// ListMembers implements the operation of the same name.
func (h *Handlers) ListMembers(ctx context.Context, r api.ListMembersRequestObject) (api.ListMembersResponseObject, error) {
	ctx, err := out(ctx)
	if err != nil {
		return nil, err
	}
	size, token := pageArgs(r.Params.Limit, r.Params.Cursor)
	resp, err := h.up.Members.ListMembers(ctx, &cpv1.ListMembersRequest{OrganizationId: r.OrgId.String(), PageSize: size, PageToken: token})
	if err != nil {
		return nil, err
	}
	body := api.MemberList{Items: make([]api.Member, 0, len(resp.GetMembers())), Page: page(resp.GetNextPageToken())}
	for _, m := range resp.GetMembers() {
		body.Items = append(body.Items, member(m))
	}
	return api.ListMembers200JSONResponse{Body: body}, nil
}

// AddMember implements the operation of the same name.
func (h *Handlers) AddMember(ctx context.Context, r api.AddMemberRequestObject) (api.AddMemberResponseObject, error) {
	ctx, err := out(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := h.up.Members.AddMember(ctx, &cpv1.AddMemberRequest{OrganizationId: r.OrgId.String(), Email: string(r.Body.Email), Role: roleIn[r.Body.Role]})
	if err != nil {
		return nil, err
	}
	return api.AddMember201JSONResponse{Body: member(resp.GetMember())}, nil
}

// GetMember implements the operation of the same name.
func (h *Handlers) GetMember(ctx context.Context, r api.GetMemberRequestObject) (api.GetMemberResponseObject, error) {
	ctx, err := out(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := h.up.Members.GetMember(ctx, &cpv1.GetMemberRequest{OrganizationId: r.OrgId.String(), UserId: r.UserId.String()})
	if err != nil {
		return nil, err
	}
	return api.GetMember200JSONResponse{Body: member(resp.GetMember())}, nil
}

// UpdateMemberRole implements the operation of the same name.
func (h *Handlers) UpdateMemberRole(ctx context.Context, r api.UpdateMemberRoleRequestObject) (api.UpdateMemberRoleResponseObject, error) {
	ctx, err := out(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := h.up.Members.UpdateMemberRole(ctx, &cpv1.UpdateMemberRoleRequest{OrganizationId: r.OrgId.String(), UserId: r.UserId.String(), Role: roleIn[r.Body.Role]})
	if err != nil {
		return nil, err
	}
	return api.UpdateMemberRole200JSONResponse{Body: member(resp.GetMember())}, nil
}

// RemoveMember implements the operation of the same name.
func (h *Handlers) RemoveMember(ctx context.Context, r api.RemoveMemberRequestObject) (api.RemoveMemberResponseObject, error) {
	ctx, err := out(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := h.up.Members.RemoveMember(ctx, &cpv1.RemoveMemberRequest{OrganizationId: r.OrgId.String(), UserId: r.UserId.String()}); err != nil {
		return nil, err
	}
	return api.RemoveMember204Response{}, nil
}

// ---- projects

// ListProjects implements the operation of the same name.
func (h *Handlers) ListProjects(ctx context.Context, r api.ListProjectsRequestObject) (api.ListProjectsResponseObject, error) {
	ctx, err := out(ctx)
	if err != nil {
		return nil, err
	}
	size, token := pageArgs(r.Params.Limit, r.Params.Cursor)
	resp, err := h.up.Projects.ListProjects(ctx, &cpv1.ListProjectsRequest{OrganizationId: r.OrgId.String(), PageSize: size, PageToken: token})
	if err != nil {
		return nil, err
	}
	body := api.ProjectList{Items: make([]api.Project, 0, len(resp.GetProjects())), Page: page(resp.GetNextPageToken())}
	for _, p := range resp.GetProjects() {
		body.Items = append(body.Items, project(p))
	}
	return api.ListProjects200JSONResponse{Body: body}, nil
}

// CreateProject implements the operation of the same name.
func (h *Handlers) CreateProject(ctx context.Context, r api.CreateProjectRequestObject) (api.CreateProjectResponseObject, error) {
	ctx, err := out(withIdem(ctx, r.Params.IdempotencyKey))
	if err != nil {
		return nil, err
	}
	req := &cpv1.CreateProjectRequest{OrganizationId: r.OrgId.String(), Name: r.Body.Name, Slug: r.Body.Slug}
	if r.Body.Description != nil {
		req.Description = *r.Body.Description
	}
	resp, err := h.up.Projects.CreateProject(ctx, req)
	if err != nil {
		return nil, err
	}
	return api.CreateProject201JSONResponse{Body: project(resp.GetProject())}, nil
}

// GetProject implements the operation of the same name.
func (h *Handlers) GetProject(ctx context.Context, r api.GetProjectRequestObject) (api.GetProjectResponseObject, error) {
	ctx, err := out(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := h.up.Projects.GetProject(ctx, &cpv1.GetProjectRequest{OrganizationId: r.OrgId.String(), ProjectId: r.ProjectId.String()})
	if err != nil {
		return nil, err
	}
	return api.GetProject200JSONResponse{Body: project(resp.GetProject())}, nil
}

// UpdateProject implements the operation of the same name.
func (h *Handlers) UpdateProject(ctx context.Context, r api.UpdateProjectRequestObject) (api.UpdateProjectResponseObject, error) {
	ctx, err := out(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := h.up.Projects.UpdateProject(ctx, &cpv1.UpdateProjectRequest{OrganizationId: r.OrgId.String(), ProjectId: r.ProjectId.String(),
		Name: r.Body.Name, Slug: r.Body.Slug, Description: r.Body.Description})
	if err != nil {
		return nil, err
	}
	return api.UpdateProject200JSONResponse{Body: project(resp.GetProject())}, nil
}

// DeleteProject implements the operation of the same name.
func (h *Handlers) DeleteProject(ctx context.Context, r api.DeleteProjectRequestObject) (api.DeleteProjectResponseObject, error) {
	ctx, err := out(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := h.up.Projects.DeleteProject(ctx, &cpv1.DeleteProjectRequest{OrganizationId: r.OrgId.String(), ProjectId: r.ProjectId.String()}); err != nil {
		return nil, err
	}
	return api.DeleteProject204Response{}, nil
}

// ---- resources

// ListResources implements the operation of the same name.
func (h *Handlers) ListResources(ctx context.Context, r api.ListResourcesRequestObject) (api.ListResourcesResponseObject, error) {
	ctx, err := out(ctx)
	if err != nil {
		return nil, err
	}
	size, token := pageArgs(r.Params.Limit, r.Params.Cursor)
	req := &cpv1.ListResourcesRequest{OrganizationId: r.OrgId.String(), ProjectId: r.ProjectId.String(), PageSize: size, PageToken: token}
	if r.Params.Kind != nil {
		req.Kind = kindIn[*r.Params.Kind]
	}
	resp, err := h.up.Resources.ListResources(ctx, req)
	if err != nil {
		return nil, err
	}
	body := api.ResourceList{Items: make([]api.Resource, 0, len(resp.GetResources())), Page: page(resp.GetNextPageToken())}
	for _, x := range resp.GetResources() {
		body.Items = append(body.Items, resource(x))
	}
	return api.ListResources200JSONResponse{Body: body}, nil
}

// CreateResource implements the operation of the same name.
func (h *Handlers) CreateResource(ctx context.Context, r api.CreateResourceRequestObject) (api.CreateResourceResponseObject, error) {
	ctx, err := out(withIdem(ctx, r.Params.IdempotencyKey))
	if err != nil {
		return nil, err
	}
	req := &cpv1.CreateResourceRequest{OrganizationId: r.OrgId.String(), ProjectId: r.ProjectId.String(), Kind: kindIn[r.Body.Kind], Name: r.Body.Name}
	if r.Body.Labels != nil {
		req.Labels = *r.Body.Labels
	}
	resp, err := h.up.Resources.CreateResource(ctx, req)
	if err != nil {
		return nil, err
	}
	return api.CreateResource201JSONResponse{Body: resource(resp.GetResource())}, nil
}

// GetResource implements the operation of the same name.
func (h *Handlers) GetResource(ctx context.Context, r api.GetResourceRequestObject) (api.GetResourceResponseObject, error) {
	ctx, err := out(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := h.up.Resources.GetResource(ctx, &cpv1.GetResourceRequest{OrganizationId: r.OrgId.String(), ProjectId: r.ProjectId.String(), ResourceId: r.ResourceId.String()})
	if err != nil {
		return nil, err
	}
	return api.GetResource200JSONResponse{Body: resource(resp.GetResource())}, nil
}

// UpdateResource implements the operation of the same name.
func (h *Handlers) UpdateResource(ctx context.Context, r api.UpdateResourceRequestObject) (api.UpdateResourceResponseObject, error) {
	ctx, err := out(ctx)
	if err != nil {
		return nil, err
	}
	req := &cpv1.UpdateResourceRequest{OrganizationId: r.OrgId.String(), ProjectId: r.ProjectId.String(), ResourceId: r.ResourceId.String(), Name: r.Body.Name}
	if r.Body.Labels != nil {
		req.Labels = &cpv1.Labels{Values: *r.Body.Labels}
	}
	resp, err := h.up.Resources.UpdateResource(ctx, req)
	if err != nil {
		return nil, err
	}
	return api.UpdateResource200JSONResponse{Body: resource(resp.GetResource())}, nil
}

// DeleteResource implements the operation of the same name.
func (h *Handlers) DeleteResource(ctx context.Context, r api.DeleteResourceRequestObject) (api.DeleteResourceResponseObject, error) {
	ctx, err := out(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := h.up.Resources.DeleteResource(ctx, &cpv1.DeleteResourceRequest{OrganizationId: r.OrgId.String(), ProjectId: r.ProjectId.String(), ResourceId: r.ResourceId.String()}); err != nil {
		return nil, err
	}
	return api.DeleteResource204Response{}, nil
}

// ---- API keys

// ListApiKeys implements the operation of the same name.
func (h *Handlers) ListApiKeys(ctx context.Context, r api.ListApiKeysRequestObject) (api.ListApiKeysResponseObject, error) { //nolint:revive // name fixed by the generated interface
	ctx, err := out(ctx)
	if err != nil {
		return nil, err
	}
	size, token := pageArgs(r.Params.Limit, r.Params.Cursor)
	req := &cpv1.ListApiKeysRequest{OrganizationId: r.OrgId.String(), PageSize: size, PageToken: token}
	if r.Params.ProjectId != nil {
		req.ProjectId = r.Params.ProjectId.String()
	}
	resp, err := h.up.APIKeys.ListApiKeys(ctx, req)
	if err != nil {
		return nil, err
	}
	body := api.ApiKeyList{Items: make([]api.ApiKey, 0, len(resp.GetApiKeys())), Page: page(resp.GetNextPageToken())}
	for _, k := range resp.GetApiKeys() {
		body.Items = append(body.Items, apiKey(k))
	}
	return api.ListApiKeys200JSONResponse{Body: body}, nil
}

// CreateApiKey implements the operation of the same name.
func (h *Handlers) CreateApiKey(ctx context.Context, r api.CreateApiKeyRequestObject) (api.CreateApiKeyResponseObject, error) { //nolint:revive // name fixed by the generated interface
	ctx, err := out(withIdem(ctx, r.Params.IdempotencyKey))
	if err != nil {
		return nil, err
	}
	req := &cpv1.CreateApiKeyRequest{OrganizationId: r.OrgId.String(), ProjectId: r.Body.ProjectId.String(), Name: r.Body.Name}
	for _, s := range r.Body.Scopes {
		req.Scopes = append(req.Scopes, scopeIn[s])
	}
	if r.Body.ExpireTime != nil {
		req.ExpireTime = timestamppb.New(*r.Body.ExpireTime)
	}
	resp, err := h.up.APIKeys.CreateApiKey(ctx, req)
	if err != nil {
		return nil, err
	}
	body := api.CreatedApiKey{ApiKey: apiKey(resp.GetApiKey())}
	if s := resp.GetSecret(); s != "" {
		body.Secret = &s
	}
	return api.CreateApiKey201JSONResponse{Body: body}, nil
}

// GetApiKey implements the operation of the same name.
func (h *Handlers) GetApiKey(ctx context.Context, r api.GetApiKeyRequestObject) (api.GetApiKeyResponseObject, error) { //nolint:revive // name fixed by the generated interface
	ctx, err := out(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := h.up.APIKeys.GetApiKey(ctx, &cpv1.GetApiKeyRequest{OrganizationId: r.OrgId.String(), ApiKeyId: r.ApiKeyId.String()})
	if err != nil {
		return nil, err
	}
	return api.GetApiKey200JSONResponse{Body: apiKey(resp.GetApiKey())}, nil
}

// RevokeApiKey implements the operation of the same name.
func (h *Handlers) RevokeApiKey(ctx context.Context, r api.RevokeApiKeyRequestObject) (api.RevokeApiKeyResponseObject, error) { //nolint:revive // name fixed by the generated interface
	ctx, err := out(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := h.up.APIKeys.RevokeApiKey(ctx, &cpv1.RevokeApiKeyRequest{OrganizationId: r.OrgId.String(), ApiKeyId: r.ApiKeyId.String()})
	if err != nil {
		return nil, err
	}
	return api.RevokeApiKey200JSONResponse{Body: apiKey(resp.GetApiKey())}, nil
}

// ---- audit

// ListAuditEvents implements the operation of the same name.
func (h *Handlers) ListAuditEvents(ctx context.Context, r api.ListAuditEventsRequestObject) (api.ListAuditEventsResponseObject, error) {
	ctx, err := out(ctx)
	if err != nil {
		return nil, err
	}
	size, token := pageArgs(r.Params.Limit, r.Params.Cursor)
	resp, err := h.up.Audit.ListAuditEvents(ctx, &cpv1.ListAuditEventsRequest{OrganizationId: r.OrgId.String(), PageSize: size, PageToken: token})
	if err != nil {
		return nil, err
	}
	body := api.AuditEventList{Items: make([]api.AuditEvent, 0, len(resp.GetEvents())), Page: page(resp.GetNextPageToken())}
	for _, e := range resp.GetEvents() {
		body.Items = append(body.Items, auditEvent(e))
	}
	return api.ListAuditEvents200JSONResponse{Body: body}, nil
}
