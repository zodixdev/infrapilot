// Package grpcmeta names the gRPC metadata keys the edge uses to pass request context to internal services
// (ADR-0025), so producer (api-gateway) and consumers agree on the spelling.
package grpcmeta

const (
	// Principal carries the serialized infrapilot.controlplane.v1.Principal of the authenticated caller. Binary
	// ("-bin") metadata; accepted only from allow-listed edge peers.
	Principal = "x-infrapilot-principal-bin"
	// IdempotencyKey carries the client's Idempotency-Key header (ADR-0024).
	IdempotencyKey = "x-infrapilot-idempotency-key"
	// RequestID carries the edge request id, recorded in logs and audit events.
	RequestID = "x-request-id"
)
