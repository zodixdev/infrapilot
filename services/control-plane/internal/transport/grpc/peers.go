package grpc

import (
	cpv1 "github.com/zodixdev/infrapilot/gen/go/infrapilot/controlplane/v1"
)

// PeerRules is the control-plane allow-list (ADR-0025): every service serves the edge only. Internal callers of
// CheckPermission are added by the milestone that introduces them, together with how they obtain a principal
// (an ADR), because the principal interceptor accepts principals from edge peers only.
func PeerRules(edge []string) map[string][]string {
	edgeOnly := []string{
		cpv1.OrganizationService_ServiceDesc.ServiceName,
		cpv1.ProjectService_ServiceDesc.ServiceName,
		cpv1.MemberService_ServiceDesc.ServiceName,
		cpv1.ResourceService_ServiceDesc.ServiceName,
		cpv1.ApiKeyService_ServiceDesc.ServiceName,
		cpv1.AuditService_ServiceDesc.ServiceName,
	}
	rules := make(map[string][]string, len(edgeOnly)+2)
	for _, svc := range edgeOnly {
		rules["/"+svc+"/*"] = edge
	}
	rules["/"+cpv1.AuthorizationService_ServiceDesc.ServiceName+"/*"] = edge
	return rules
}
