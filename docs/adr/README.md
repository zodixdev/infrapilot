# Architecture Decision Records

All ADRs below are **accepted** (baseline v1.0). To change one, add a new ADR that supersedes it.
New decisions made during implementation: write a new ADR (status `accepted`, decider `claude-senior`),
and list it in the milestone report so the owner can review it later.

| ADR | Decision |
|---|---|
| [0001](0001-monorepo.md) | Monorepo, `go.work`, one Go module per service |
| [0002](0002-service-boundaries.md) | 12 deployables with DB-per-service ownership |
| [0003](0003-api-styles.md) | REST/OpenAPI public, gRPC internal, gRPC streams for agents (OpenAPI version: see 0020) |
| [0004](0004-kafka-and-rabbitmq.md) | Kafka for events, RabbitMQ for tasks |
| [0005](0005-outbox-idempotency.md) | Transactional outbox + idempotent consumers |
| [0006](0006-event-schemas.md) | Protobuf events with buf breaking-change checks |
| [0007](0007-timeseries-and-analytics-storage.md) | TimescaleDB for metrics, ClickHouse for logs/spans/events |
| [0008](0008-go-data-access.md) | pgx + sqlc + goose, no ORM |
| [0009](0009-identity.md) | Keycloak OIDC for users, mTLS internal CA for agents |
| [0010](0010-multi-tenancy.md) | Shared schema + `organization_id` + Postgres RLS |
| [0011](0011-runtime-and-deployment.md) | Kubernetes + Helm everywhere (kind locally), Terraform for cloud |
| [0012](0012-observability.md) | OpenTelemetry → Prometheus, Tempo, Loki, Grafana |
| [0013](0013-check-scheduling.md) | Lease-based distributed check scheduling in Postgres |
| [0014](0014-workflow-engine.md) | Custom durable workflow engine (not Temporal) |
| [0015](0015-ai-boundary.md) | AI is advisory; deterministic evidence first, LLM summarizes |
| [0016](0016-go-service-stack.md) | Go libraries and service template |
| [0017](0017-frontend.md) | Next.js App Router + Auth.js (Keycloak) |
| [0018](0018-postgres-roles-and-tenant-context.md) | Three PostgreSQL roles per service; tenant context set per transaction (refines 0008/0010) |
| [0019](0019-self-telemetry-wiring.md) | Self-telemetry wiring: metrics pulled from /metrics, traces pushed over OTLP (refines 0012) |
| [0020](0020-openapi-document-version.md) | Public OpenAPI document stays on 3.0.3 until the generator proves 3.1 support (partially supersedes 0003) |
| [0021](0021-tool-pinning.md) | Toolchain pinned with Go `tool` directives + Brewfile |
| [0022](0022-rbac-and-authorization.md) | RBAC matrix, API-key scopes; owning service authoritative, gateway pre-check cached (refines 0009) |
| [0023](0023-user-scoped-read-context.md) | Second RLS dimension `app.user_id` + `WithUserTx` for "my organizations" (extends 0018) |
| [0024](0024-idempotency-keys.md) | `Idempotency-Key` enforced by the owning service in the creating transaction (refines 0003) |
| [0025](0025-service-identity.md) | mTLS SPIFFE-style identities, per-RPC peer allow-list, principal accepted only from the edge (refines 0009) |
| [0026](0026-gateway-edge.md) | Gateway JWKS cache, API-key/permission caches, GCRA rate limiting failing open |
| [0027](0027-control-plane-retention.md) | Soft delete + purge CronJob under the batch role; retention per table |
| [0028](0028-idempotency-fingerprint.md) | Stable idempotency fingerprints from control-plane service inputs (refines 0024) |
| [0029](0029-otlp-ingestion-milestones.md) | OTLP metrics in M03, agent identity in M05, and logs and traces in M06 |
| [0030](0030-tenant-isolated-timescale-storage.md) | Timescale rowstore RLS, transactional rollups, TOAST compression and owner-served reads (refines 0007) |
| [0031](0031-ordered-resource-state.md) | Transactional resource lifecycle capture, ordered delivery and compacted-topic recovery (refines 0005/0018) |

Template: [0000-template.md](0000-template.md)
