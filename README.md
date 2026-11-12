# InfraPilot

Multi-tenant infrastructure monitoring, incident management and automation platform.

**Stack:** Go · Python/FastAPI · gRPC/Protobuf · Kafka · RabbitMQ · PostgreSQL · TimescaleDB · ClickHouse ·
Redis · MinIO/S3 · Keycloak · Kubernetes/Helm · OpenTelemetry/Prometheus/Grafana/Tempo/Loki · Next.js

> Status: architecture baseline accepted; implementation starts at M01. See [roadmap](docs/roadmap.md).

| Doc | Purpose |
|---|---|
| [Architecture overview](docs/architecture/overview.md) | services, communication, SLOs, capacity |
| [Data & messaging](docs/architecture/data.md) | ownership, topics, queues, guarantees |
| [Security](docs/architecture/security.md) | identities, remote actions, SSRF, threat model |
| [Failure scenarios](docs/architecture/failure-scenarios.md) | F01–F18 and how they are tested |
| [ADRs](docs/adr/README.md) | accepted decisions |
| [Roadmap](docs/roadmap.md) | milestones and acceptance criteria |
| [Job coverage](docs/job-coverage.md) | skills → where they are proven |
