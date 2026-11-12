# ADR-0012: OpenTelemetry-first self-observability

- Status: accepted · Decider: senior-backend · Date: 2026-11-12

## Decision
Every service uses the OTel Go/Python SDK, exports OTLP to an in-cluster OTel Collector, which routes:
metrics → Prometheus (remote write), traces → Tempo, logs → Loki. Grafana dashboards and Prometheus
alert rules are provisioned from `observability/` as code. Trace context propagates over HTTP, gRPC,
Kafka headers and AMQP headers. Logs are slog JSON with `trace_id`/`span_id`.

Required per service: RED metrics (rate, errors, duration) per endpoint/RPC, Go runtime metrics,
pool metrics, consumer lag / queue depth where applicable, outbox pending age.

## Consequences
Platform telemetry never mixes with customer telemetry (ClickHouse/Timescale). Each milestone ships
its dashboards + alerts + runbooks, not just code.
