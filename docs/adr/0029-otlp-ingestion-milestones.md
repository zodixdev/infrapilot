# ADR-0029 — Stage OTLP ingestion by signal and caller identity

- Status: accepted (supersedes the ingest-scope timing in ADR-0022)
- Date: 2026-11-16
- Decider: claude-senior

## Context

M03 requires a real ingestion response during a Kafka outage (F04). M05 agents also need a receiver for
host metrics. The original roadmap introduced the OTLP receiver in M06, after both requirements. The
architecture already defines an OTLP receiver, project API keys and agent certificate authentication.

## Options considered

1. Introduce the entire receiver in M03. This covers all signals early and also introduces dependencies
   on ClickHouse and agent enrollment ahead of their milestones.
2. Test the broker client in M03 and add the receiver in M06. This leaves F04's transport behavior untested
   in M03 and delays a usable host-metrics path for M05.
3. Introduce authenticated OTLP metrics in M03 and extend the same receiver as the next storage backends
   and caller identities become available.

## Decision

Use option 3. M03 serves OTLP metrics over gRPC and HTTP. The caller supplies its project API key and a
resource UUID in `x-infrapilot-resource-id` on both transports. A dedicated control-plane RPC receives the
key's SHA-256 digest and the resource UUID. Its mTLS peer allow-list admits telemetry-ingestion only.
Control-plane verifies the live key, its ingest scope, and the live resource in the key's project.
The RPC returns the verified organization and project. OTLP payload attributes cannot select the tenant.

Apply per-tenant quotas and cap request size, point count and concurrent publication. After validation, write
`telemetry.metrics.v1` envelopes to Kafka. A successful export response requires acknowledgment for each
record in the request. Use stable sample identities for retries because a failed response can follow partial
broker acknowledgment. When capacity is exhausted, return HTTP 503 with `Retry-After` or gRPC
`RESOURCE_EXHAUSTED` with `RetryInfo`. An unavailable broker that cannot acknowledge a request before its
deadline returns a retryable error without a success response. [OTLP specification](https://opentelemetry.io/docs/specs/otlp/)

M05 adds authentication for enrolled agents and verifies agent telemetry through the existing metrics path.
M06 adds OTLP logs and traces, their storage path, and independent backpressure. The M06 F04 test covers
those signals.

## Consequences and verification

M03 gains a deployable and a real authenticated Kafka producer independent of the check scheduler. It can
verify outage responses and the metrics path before an agent exists. M05 and M06 extend the accepted
receiver contracts. Test both M03 transports and tenant boundaries with real control-plane, Redis and
Kafka, including rejecting unauthenticated and foreign-resource writes and recovering from F04.
