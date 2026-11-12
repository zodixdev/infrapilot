# Data, Messaging & Consistency

## 1. Ownership

| Dataset | Owner | Store | Source of truth | Retention | Consistency |
|---|---|---|---|---|---|
| Orgs, projects, members, roles, API keys, resources | control-plane | PostgreSQL `controlplane` | yes | until deleted (+30d soft delete) | strong (tx) |
| Check definitions, leases, last state | check-runner | PostgreSQL `checks` | yes | until deleted | strong |
| Check results (raw) | stream-processor | TimescaleDB `check_results` hypertable | yes | 7d raw, 90d 5-min rollups | eventual (≤10 s) |
| Host & app metrics | stream-processor | TimescaleDB `metrics` hypertable | yes | 7d raw, 30d 1-min, 400d 1-h rollups | eventual |
| Logs, spans, raw events | stream-processor | ClickHouse (`logs`, `spans`, `events`) | yes | 14d (TTL), archive to S3 optional | eventual |
| Alert rules, incidents, timeline | incident-engine | PostgreSQL `incidents` | yes | 400d | strong |
| Channels, routes, delivery attempts | notification-service | PostgreSQL `notifications` | yes | attempts 30d | strong |
| Agents, certs metadata, heartbeats (last) | agent-manager | PostgreSQL `agents` | yes | until deleted | strong |
| Workflows, approvals, audit log | workflow-engine | PostgreSQL `workflows` | yes | audit 400d, append-only | strong |
| Investigation reports | intelligence-service | PostgreSQL `intelligence` + S3 bundles | yes | 400d | strong |
| Rate-limit counters, quota buckets, permission cache | gateway / ingestion | Redis | **no** (rebuildable) | TTL ≤ 1h | best-effort |

Rules: one PostgreSQL cluster in prod, **one database + one DB role per service**. No cross-database queries.
Other services get data only through gRPC or events.

## 2. Multi-tenancy

- Every tenant row has `organization_id UUID NOT NULL`, indexed first in composite indexes.
- Repository layer requires `orgID` on every tenant-scoped method (compile-time: methods take `tenant.ID`).
- **Defense in depth:** PostgreSQL Row-Level Security on tenant tables using `current_setting('app.org_id')`,
  set with `SET LOCAL` inside each transaction. Service DB roles are not table owners and do not bypass RLS.
- TimescaleDB/ClickHouse queries go only through query-service, which injects `organization_id` filters
  and enforces time-range and row limits.
- Every service has a cross-tenant access test suite.

## 3. Kafka topics

All topics: replication factor 3 (prod), `min.insync.replicas=2`, producers `acks=all`, idempotent producer on.

| Topic | Key | Partitions (prod) | Retention | Producer → Consumers |
|---|---|---|---|---|
| `check.results.v1` | `org_id/check_id` | 24 | 3d | check-runner → stream-processor, incident-engine |
| `telemetry.metrics.v1` | `org_id/resource_id` | 48 | 1d | ingestion → stream-processor |
| `telemetry.logs.v1` | `org_id/resource_id` | 48 | 1d | ingestion → stream-processor |
| `telemetry.spans.v1` | `org_id/trace_id` | 48 | 1d | ingestion → stream-processor |
| `agent.events.v1` | `org_id/agent_id` | 12 | 7d | agent-manager → incident-engine, control-plane |
| `signals.v1` | `org_id/resource_id` | 24 | 3d | stream-processor → incident-engine |
| `resource.state.v1` | `org_id/resource_id` | 12 | compacted | control-plane → check-runner, incident-engine |
| `incident.lifecycle.v1` | `org_id/incident_id` | 12 | 30d | incident-engine → workflow-engine, (future) analytics |

Ordering is guaranteed only per key. Consumers commit offsets **after** durable processing (at-least-once).
Poison messages → `<topic>.dlq` after N attempts with error metadata headers.

## 4. RabbitMQ queues

Quorum queues, publisher confirms, manual acks, `prefetch` tuned per consumer.

| Queue | Producer | Consumer | Retry tiers | DLQ |
|---|---|---|---|---|
| `notifications.deliver` | incident-engine (outbox) | notification-service | 10s → 1m → 10m → 1h | `notifications.deliver.dlq` |
| `investigations.run` | incident-engine (outbox) | intelligence-service | 30s → 5m | `investigations.run.dlq` |
| `workflows.step` | workflow-engine (outbox) | workflow-engine workers | 10s → 1m → 10m | `workflows.step.dlq` |

Retries use per-tier delay queues (`x-message-ttl` + dead-letter back to the main exchange).
Attempt count is carried in a header; exceeding the max routes to DLQ and raises an alert.

## 5. Delivery guarantees

- **Outbox:** state change + `outbox` row in the same transaction. A dispatcher (inside the owning service,
  leader-free, `FOR UPDATE SKIP LOCKED`) publishes and marks rows sent. Publish failures leave rows pending.
- **Idempotent consumers:** every message carries `event_id`/`message_id`. Consumers record processed IDs in a
  `processed_messages` table in the same transaction as the side effect (unique constraint), or use natural
  unique keys (e.g. one incident per `(org_id, rule_id, fingerprint, open)`).
- **External side effects** (sending a notification, running a command) use idempotency keys and attempt records;
  we guarantee at-least-once delivery with dedup at the receiver where possible; webhooks include
  `X-InfraPilot-Delivery-Id` so receivers can dedupe.

## 6. Event envelope

Defined in `contracts/proto/infrapilot/events/v1/envelope.proto`:

```
event_id (UUIDv7) · event_type · schema_version · organization_id · subject_id
occurred_at · produced_at · producer · trace_parent · payload (Any)
```

## 7. Migrations

`goose` SQL migrations per service, run as a Kubernetes Job (Helm pre-upgrade hook) before rollout.
Expand → migrate → contract: never drop/rename a column in the same release that stops using it.
