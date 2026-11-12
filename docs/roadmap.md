# InfraPilot Roadmap — Production Milestones

Every milestone delivers **production-grade components**: real implementation, automated tests against
real dependencies, observability (metrics, traces, logs, dashboards, alerts), Helm deployment, runbooks,
and its failure scenarios (`architecture/failure-scenarios.md`) proven by tests.
"Acceptance" below means **automated checks that pass in `make verify`**, not a manual demo.

Status values: `not started` · `in progress` · `done`. Update this file when a milestone changes state.

---

## M01 — Engineering platform · status: not started

**Scope**
- Repo skeleton per ADR-0001/0016: `go.work`, `pkg/` module, `gen/`, `contracts/`, `deploy/`, `observability/`, `tests/`.
- `Makefile`: `bootstrap`, `gen`, `lint`, `test`, `test-integration`, `up`/`down` (compose profiles), `cluster`, `deploy`, `verify`.
- Tooling pinned: golangci-lint config, buf (lint + breaking), sqlc, goose, oapi-codegen, pre-commit (gofmt, gitleaks).
- `pkg/`: config, slog logger + redaction, OTel bootstrap, HTTP & gRPC servers with graceful shutdown,
  health/readiness registry, pgx pool + `WithTx` (+ `SET LOCAL app.org_id`), outbox table + dispatcher,
  Kafka producer/consumer framework (retries, DLQ, lag metrics), AMQP publisher (confirms) / consumer (tiers, DLQ),
  SSRF-safe dialer. All with unit + testcontainers integration tests.
- `contracts/proto/infrapilot/events/v1/envelope.proto` + buf config; `contracts/openapi/public.yaml` skeleton
  (info, security schemes, error model, pagination conventions).
- docker compose with profiles: `core` (postgres, redis, keycloak), `stream` (kafka KRaft, timescaledb),
  `tasks` (rabbitmq), `analytics` (clickhouse, minio), `obs` (otel-collector, prometheus, tempo, loki, grafana).
- kind cluster config + Helm library chart `infrapilot-lib` (Deployment, Service, PDB, HPA, probes,
  resources, NetworkPolicy, ServiceMonitor, migration Job hook) + kubeconform tests.
- Observability as code: collector config, Prometheus rules, Grafana provisioning, base "Go service" dashboard.
- GitHub Actions workflows (lint, test, integration, buf breaking, govulncheck, gitleaks, build + trivy, helm lint, kind e2e) — runnable locally with `act` where possible.

**Acceptance**
- `make bootstrap && make verify` passes on a clean machine (lint, unit, race, integration).
- `make up PROFILE=all` brings all dependencies healthy; `make cluster deploy` installs a chart built from the library chart and passes `helm test`.
- `pkg/` integration tests prove: outbox survives broker outage (F08 pattern), consumer dedup on redelivery (F06 pattern), graceful shutdown drains in-flight work, SSRF dialer blocks the forbidden ranges incl. DNS-rebinding test.

## M02 — Identity, control plane, gateway · status: not started

**Scope:** Keycloak realm as code; control-plane (orgs, projects, memberships, roles, resources, API keys,
`CheckPermission` gRPC) with RLS; api-gateway (JWT/JWKS, API keys, Redis rate limiting, routing, problem+json,
`Idempotency-Key`); OpenAPI for these endpoints; audit of membership/role changes.

**Acceptance:** CRUD + RBAC matrix tests for every role; cross-tenant suite (API and direct DB with RLS);
F01, F02, F03 tests; `EXPLAIN ANALYZE` for list endpoints; k6 smoke test p99 < 300 ms at 200 rps on kind;
dashboards + alerts + runbooks for both services.

## M03 — Checks & telemetry pipeline · status: not started

**Scope:** check-runner (HTTP/TCP/TLS-expiry/DNS/ICMP checks, lease scheduler ADR-0013, worker pool,
SSRF guard, per-tenant limits); Kafka topics as code; stream-processor (batched writes to TimescaleDB,
hypertables, continuous aggregates, retention/compression, threshold rules → `signals.v1`); query-service
(metrics/check-results read API, tenant-scoped, bounded); public API for checks and results.

**Acceptance:** F04, F05, F06, F07 tests; scheduling drift p95 < 5 s with 10k checks / 3 replicas on kind;
pprof + trace captured under load with findings in the report; consumer lag dashboard + alerts.

## M04 — Incidents & notifications · status: not started

**Scope:** incident-engine (alert rules, correlation/fingerprinting, dedup, state machine
`triggered → acknowledged → resolved` (+ `reopened`), timeline, outbox → RabbitMQ + `incident.lifecycle.v1`);
notification-service (email/Slack/Telegram/webhook with HMAC, routing rules, delivery attempts, tiered retries, DLQ).

**Acceptance:** F06, F08, F09, F10 tests; e2e: failing check → exactly one incident and one notification per channel,
recovery → resolved; state-machine property tests; detection p95 < 30 s and notify p95 < 15 s measured.

## M05 — Agents · status: not started

**Scope:** agent-manager (enrollment tokens, internal CA, CSR signing, rotation, revocation, bidi stream,
heartbeats, `agent.events.v1`); infra-agent (host metrics via OTLP, systemd unit status, disk-buffered telemetry,
reconnect with backoff+jitter, signed-command verification stub enforced from day one); packaging (static binary,
systemd unit, install script, container image).

**Acceptance:** F11, F12 tests; 1,000 simulated agents against one agent-manager replica on kind with
CPU/memory/goroutine numbers recorded; no goroutine leak after 1,000 connect/disconnect cycles (goleak + pprof).

## M06 — Logs & traces · status: not started

**Scope:** telemetry-ingestion (OTLP gRPC/HTTP, API key + mTLS auth, quotas, backpressure); ClickHouse schemas
with TTL; stream-processor logs/spans writers; query-service log search + trace lookup; S3 archive for raw batches.

**Acceptance:** F04, F13 tests; sustained 20k events/s on kind with p95 queryable < 10 s; ADR-0007 benchmark
report (Timescale vs ClickHouse) written.

## M07 — Dashboard · status: not started

**Scope:** Next.js app per ADR-0017: login, org/project switcher, resources, checks, metrics charts, logs search,
incidents list + timeline, notification settings, agents, API keys.

**Acceptance:** Playwright e2e for core journeys; type-check + lint clean; accessibility checks (axe) without
serious violations; Lighthouse performance ≥ 90 on main pages; screenshots captured for the README.

## M08 — Workflows & remote actions · status: not started

**Scope:** workflow-engine (definitions, executions, approvals, policies, timeouts, retries, compensation,
Ed25519 signing, audit log); agent action executors (allow-listed); public API + dashboard pages.

**Acceptance:** F14, F15 tests; RBAC/policy matrix; audit completeness test (100% of sensitive actions);
threat-model review recorded.

## M09 — Intelligence · status: not started

**Scope:** intelligence-service per ADR-0015 (evidence collectors, analyzers, LLM adapter, schema validation,
reports, S3 bundles); incident page shows investigation.

**Acceptance:** F16 test; golden-dataset tests (seeded incidents → expected top hypothesis); prompt-injection
test cases; cost/latency metrics.

## M10 — Production hardening & release · status: not started

**Scope:** Terraform for a real cluster (provider ADR), operators install, External Secrets, TLS ingress,
backups (CNPG + object storage), DR drill, k6 load at the capacity design point, chaos experiments (Chaos Mesh),
SLO dashboards + burn-rate alerts, security review (gosec, trivy, dependency audit), capacity plan, release
process with rollback.

**Acceptance:** F17, F18 drills pass; SLOs met under design load; zero critical/high vulnerabilities;
rollback rehearsed; `docs/architecture/capacity-planning.md` with measured numbers.
