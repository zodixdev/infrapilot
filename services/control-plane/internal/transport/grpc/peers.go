package grpc

import (
	cpv1 "github.com/zodixdev/infrapilot/gen/go/infrapilot/controlplane/v1"
)

// PeerRules is the control-plane allow-list (ADR-0025): the public-API services serve the edge only; the
// authorization service also serves the internal services that pre-authorize requests (permissionCallers).
func PeerRules(edge, permissionCallers []string) map[string][]string {
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
	rules[cpv1.AuthorizationService_CheckPermission_FullMethodName] = union(edge, permissionCallers)
	// Resolving raw API keys is the edge's job alone.
	rules[cpv1.AuthorizationService_AuthenticateApiKey_FullMethodName] = edge
	return rules
}

func union(a, b []string) []string {
	out := append([]string(nil), a...)
	for _, x := range b {
		found := false
		for _, y := range out {
			found = found || x == y
		}
		if !found {
			out = append(out, x)
		}
	}
	return out
}
