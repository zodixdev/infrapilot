# InfraPilot

Multi-tenant infrastructure monitoring, incident management and automation platform.

**Stack:** Go · Python/FastAPI · gRPC/Protobuf · Kafka · RabbitMQ · PostgreSQL · TimescaleDB · ClickHouse ·
Redis · MinIO/S3 · Keycloak · Kubernetes/Helm · OpenTelemetry/Prometheus/Grafana/Tempo/Loki · Next.js

> Status: **M02 — identity, control plane, gateway** complete. Next: M03 (checks and telemetry pipeline). See [roadmap](docs/roadmap.md).

## Identity, control plane and gateway (M02)

- **control-plane** (Go, internal gRPC) — organizations, projects, memberships with an owner/admin/operator/viewer
  matrix, resource inventory, project API keys (hashed, scoped, revocable), append-only audit log and
  `CheckPermission`. Every tenant table is protected by PostgreSQL row-level security; authorization happens inside
  the same transaction as the change; creates are idempotent by `Idempotency-Key`; retention runs as an hourly job
  under a separate role (ADR-0018, 0022–0024, 0027).
- **api-gateway** (Go, public REST) — generated from the OpenAPI contract; Keycloak tokens verified locally from a
  cached JWKS (keeps working when Keycloak is down), API keys, GCRA rate limiting in Redis that fails open when Redis
  is down, request validation, edge pre-authorization with signed caches, problem+json errors (ADR-0026).
- **Service identity** — cert-manager workload certificates with SPIFFE IDs, mTLS and a per-RPC allow-list between
  services; only the edge may act for end users (ADR-0025).
- **Realm as code** — Keycloak realm with PKCE dashboard client and API audience.
- **Proof** — RBAC matrix and cross-tenant suites (service, database and HTTP level), failure tests for a Postgres
  restart, a Keycloak outage and a Redis outage, EXPLAIN plans for every list query, kind e2e with traces across both
  services, and a k6 smoke test (200 rps, p99 < 300 ms).

## Platform (M01)

- **`pkg/`** — shared infrastructure for every Go service: typed env config with production rails, slog JSON logging
  with secret redaction (stdout + OTLP), OpenTelemetry bootstrap (traces, metrics pull + push, logs), HTTP and gRPC
  servers (RED metrics, recovery, body limits, TLS/mTLS) with deadline-bounded graceful shutdown, health/readiness
  registry, pgx pool with tenant-scoped transactions and Postgres RLS (ADR-0018).
- **Contracts** — Protobuf event `Envelope` (buf lint + breaking) and the public OpenAPI 3.0.3 skeleton (ADR-0020).
- **Local stack** — docker compose profiles `core` (Postgres, Redis, Keycloak) and `obs` (OTel Collector, Prometheus,
  Tempo, Loki, Grafana), bound to localhost.
- **Kubernetes** — kind cluster, Helm library chart `infrapilot-lib` (hardened pods, default-deny NetworkPolicy, PDB,
  HPA, ServiceMonitor, migration hook, helm tests) exercised by a conformance workload.
- **Observability as code** — collector pipelines, alert rules with SLO burn-rate alerts and promtool tests, Grafana
  provisioning and the base Go service dashboard, platform runbook.
- **CI** — lint, race tests, integration (testcontainers), govulncheck, gitleaks, image build + SBOM + Trivy,
  kind deploy + e2e; actions pinned by SHA.

## Quick start

Requires Docker (≥ 8 GB RAM), Go, kubectl and Homebrew (macOS) — everything else is pinned and installed by bootstrap.

```bash
make bootstrap                 # pinned toolchain + git hooks
make verify                    # lint, unit + race, integration (needs Docker)
make up PROFILE=core,obs       # local dependencies (secrets generated into .env)
make cluster deploy e2e        # kind + conformance chart + helm test + telemetry e2e
make kind-platform kind-deps   # cert-manager + internal CA; Postgres, Redis, Keycloak in kind
make deploy-services           # control-plane + api-gateway (images, migrations, helm tests)
make e2e-services load-smoke   # API/trace/metrics e2e and the k6 smoke test (gateway on 127.0.0.1:28080)
```


| Doc | Purpose |
|---|---|
| [Architecture overview](docs/architecture/overview.md) | services, communication, SLOs, capacity |
| [Data & messaging](docs/architecture/data.md) | ownership, topics, queues, guarantees |
| [Security](docs/architecture/security.md) | identities, remote actions, SSRF, threat model |
| [Failure scenarios](docs/architecture/failure-scenarios.md) | F01–F18 and how they are tested |
| [ADRs](docs/adr/README.md) | accepted decisions |
| [Roadmap](docs/roadmap.md) | milestones and acceptance criteria |
| [Job coverage](docs/job-coverage.md) | skills → where they are proven |
