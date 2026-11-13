# InfraPilot

Multi-tenant infrastructure monitoring, incident management and automation platform.

**Stack:** Go · Python/FastAPI · gRPC/Protobuf · Kafka · RabbitMQ · PostgreSQL · TimescaleDB · ClickHouse ·
Redis · MinIO/S3 · Keycloak · Kubernetes/Helm · OpenTelemetry/Prometheus/Grafana/Tempo/Loki · Next.js

> Status: **M01 — engineering platform** complete. Services start in M02. See [roadmap](docs/roadmap.md).

## What exists today (M01)

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
