# Job requirement coverage

Maps the requirements of the 9 target job postings (Sidekick, Andersen, Shvacher, WorkWave, proxy-infra,
2GIS CDP, 2GIS Social, YADRO, OKB) to the place in InfraPilot where each is implemented and proven.

| Requirement | Where in InfraPilot | Evidence |
|---|---|---|
| Go runtime: scheduler, GC, memory model | check-runner, ingestion, agent-manager under load | pprof/trace in M03, M05, M06, M10 reports |
| goroutines, channels, worker pools, fan-in/fan-out, context | check-runner pool, stream-processor batching, agent streams | code + goleak/race tests |
| Leak/race/lock debugging (pprof, race detector, trace) | all Go services | profiling sections of reports |
| HTTP framework, gRPC/Protobuf | gateway (REST), all internal APIs, agent streams | contracts/, buf CI |
| PostgreSQL: complex queries, plans, indexes | control-plane, incident-engine, check scheduling | EXPLAIN ANALYZE in reports |
| sqlc / SQL-first | every Go service | ADR-0008 |
| TimescaleDB | metrics + check results | M03, ADR-0007 benchmark |
| ClickHouse | logs, spans, events | M06 |
| Redis: caching, rate limiting (Streams optional) | gateway, ingestion quotas, permission cache | M02, M06 |
| Kafka, event-driven | all event topics | M03–M06 |
| RabbitMQ, retries, DLQ | notifications, investigations, workflow steps | M04, M08, M09 |
| Idempotency, outbox, Saga-like compensation | pkg/outbox, consumers, workflow compensation | F06/F08/F14 tests |
| Microservices sync/async, delivery guarantees | whole architecture | docs/architecture/data.md |
| Real-time streaming, bulk operations | telemetry pipeline, check scheduling at 50k | M03, M06 load tests |
| Docker, Kubernetes, Helm | every service, kind + cloud | M01, M10 |
| OpenTelemetry traces/metrics/logs, Prometheus, Grafana | self-observability | ADR-0012 |
| Incident diagnosis / root cause | the product itself + runbooks | M04, M09 |
| Linux, TCP/TLS/DNS, networking | check probes, agent, SSRF dialer | M03, M05 |
| Monitoring & remote management of a device fleet | agents + workflows | M05, M08 |
| Workflow engine | workflow-engine | M08 |
| Billing/usage metering (plus) | per-tenant quotas + usage counters | M02/M06 (metering), billing out of scope |
| AI agents / AI tooling | intelligence-service; development done with Claude Code subagents | M09 |
| Unit / e2e tests | every milestone | `make verify`, Playwright, k6 |
| TypeScript, React/Next.js (plus) | dashboard | M07 |

Not covered (needs a different project): DPDK/SPDK, Linux kernel modules, eBPF, storage systems (NVMeoF/iSCSI),
proxy/anti-bot engineering, OpenStack compute APIs, MongoDB.
