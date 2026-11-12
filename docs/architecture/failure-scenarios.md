# Failure Scenarios & Required Behavior

Each scenario gets an automated test (integration, e2e or chaos) in the milestone that introduces the component.

| # | Failure | Required behavior | Test type | Milestone |
|---|---|---|---|---|
| F01 | PostgreSQL restart | Pools reconnect; in-flight requests fail fast with 503; no confirmed tx lost | integration | M02 |
| F02 | Keycloak unavailable | Existing JWTs still validate (cached JWKS); new logins fail gracefully | integration | M02 |
| F03 | Redis unavailable | Rate limiting fails **open** with a metric + alert; permission cache falls back to gRPC | integration | M02 |
| F04 | Kafka unavailable | Producers buffer to a bounded limit, then ingestion returns 503/`RESOURCE_EXHAUSTED`; never ack data not stored | integration | M03 |
| F05 | check-runner pod killed or stalled mid-run | Lease expires; another worker runs the check (at-least-once); stale worker is fenced by `lease_epoch`; one effective result per `run_key`; no double incident | chaos + integration | M03 |
| F06 | Consumer crash after write, before commit | Redelivery is deduped; single row/incident | integration | M03/M04 |
| F07 | TimescaleDB slow | stream-processor applies backpressure (pauses partitions), lag alert fires, no OOM | load | M03 |
| F08 | RabbitMQ unavailable | Outbox rows accumulate; delivered after recovery; outbox-age alert | integration | M04 |
| F09 | Notification provider 5xx | Tiered retries then DLQ + alert; no duplicate sends on success | integration | M04 |
| F10 | Duplicate / out-of-order events | Idempotent processing; state machine rejects invalid transitions | unit + integration | M04 |
| F11 | Agent network partition | Agent buffers telemetry on disk (bounded); resource → `unreachable` after 3 missed heartbeats; reconnect with backoff + jitter | e2e | M05 |
| F12 | Expired / revoked agent cert | Connection refused; agent re-enrolls only with a new token | integration | M05 |
| F13 | ClickHouse unavailable | Logs path backs off; metrics path unaffected (separate consumer groups) | integration | M06 |
| F14 | Workflow engine crash mid-step | Workflow resumes from persisted state; step executed at most once per idempotency key | chaos | M08 |
| F15 | Replayed / expired command | Agent rejects; audit records rejection | unit + e2e | M08 |
| F16 | LLM provider down / slow | Investigation degrades to deterministic report; incident flow unaffected | integration | M09 |
| F17 | Node drain / rolling deploy | Zero failed public requests (PDBs, readiness, graceful shutdown) | e2e | M10 |
| F18 | Region-level data loss (simulated) | Restore from backup within RTO 1 h, RPO 15 min | DR drill | M10 |
