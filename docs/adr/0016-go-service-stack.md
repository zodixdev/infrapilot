# ADR-0016: Go libraries and service template

- Status: accepted · Decider: senior-backend · Date: 2026-11-12

| Concern | Choice |
|---|---|
| Go version | latest stable, pinned in `go.work` and CI |
| HTTP | stdlib `net/http` (1.22+ routing) + oapi-codegen strict server |
| gRPC | `google.golang.org/grpc`, buf-generated code, interceptors for auth/logging/tracing/recovery |
| Logging | `log/slog` JSON |
| Config | env vars → typed struct (`caarlos0/env`), validated at startup, fail fast |
| Postgres | pgx/v5 + sqlc + goose |
| Kafka | `twmb/franz-go` |
| RabbitMQ | `rabbitmq/amqp091-go` |
| Redis | `redis/go-redis/v9` |
| Telemetry | OpenTelemetry Go SDK + contrib instrumentations |
| Testing | stdlib `testing`, `testify/require`, `testcontainers-go`, `go test -race` |
| Lint | `golangci-lint` (strict config in repo) |
| IDs | UUIDv7 (`google/uuid`) |

Service layout:

```
services/<name>/
  cmd/<name>/main.go        # wiring only
  internal/app/             # lifecycle (start/stop), dependency wiring
  internal/domain/          # entities, state machines, pure logic (no I/O)
  internal/service/         # use cases (transactions, orchestration)
  internal/store/           # sqlc queries + repo adapters
  internal/transport/{http,grpc,kafka,amqp}/
  migrations/
  Dockerfile
```

`pkg/` (shared module) provides: config loading, logger, OTel setup, HTTP/gRPC server with graceful
shutdown, health/readiness registry, pgx pool + `WithTx`, outbox dispatcher, Kafka/AMQP consumer
frameworks with retries/DLQ, SSRF-safe dialer. **No domain logic in `pkg/`.**
