# Architecture Decision Records

All ADRs below are **accepted** (baseline v1.0). To change one, add a new ADR that supersedes it.
New decisions made during implementation: write a new ADR (status `accepted`, decider `claude-senior`),
and list it in the milestone report so the owner can review it later.

| ADR | Decision |
|---|---|
| [0001](0001-monorepo.md) | Monorepo, `go.work`, one Go module per service |
| [0002](0002-service-boundaries.md) | 12 deployables with DB-per-service ownership |
| [0003](0003-api-styles.md) | REST/OpenAPI public, gRPC internal, gRPC streams for agents |
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

Template: [0000-template.md](0000-template.md)
