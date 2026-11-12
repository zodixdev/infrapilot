# ADR-0003: REST public, gRPC internal, gRPC streams for agents

- Status: accepted · Decider: senior-backend · Date: 2026-11-12

## Decision
- Public API: REST + OpenAPI (`contracts/openapi/public.yaml`; document version 3.0.3 per ADR-0020), versioned by path `/v1`,
  cursor pagination, RFC 9457 problem+json errors, `Idempotency-Key` header on POSTs that create resources.
- Internal: gRPC + Protobuf (`contracts/proto/`), mandatory deadlines, standard status codes, retries only on
  idempotent RPCs with backoff + jitter.
- Agents: one long-lived bidirectional gRPC stream to agent-manager (heartbeats up, commands down),
  OTLP/gRPC to telemetry-ingestion.

## Alternatives rejected
GraphQL (no need for client-driven queries; harder caching/rate limiting). REST internally (no typed
contracts, no streaming). Polling agents (higher latency for commands, more load).

## Consequences
Server code for REST is generated from OpenAPI (oapi-codegen, strict server) so spec and code cannot drift.
