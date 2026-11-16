package grpc

import (
	cpv1 "github.com/zodixdev/infrapilot/gen/go/infrapilot/controlplane/v1"
)

// PeerRules separates the user-facing control-plane APIs from the telemetry credential check.
func PeerRules(edge, ingestion []string) map[string][]string {
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
	if len(ingestion) != 0 {
		rules[cpv1.IngestionAuthorizationService_ValidateIngestResource_FullMethodName] = ingestion
	}
	return rules
}
