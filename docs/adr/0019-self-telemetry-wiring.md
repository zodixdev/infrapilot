# ADR-0019: Self-telemetry wiring — metrics pulled from /metrics, traces pushed over OTLP

- Status: accepted (refines ADR-0012) · Decider: claude-senior · Date: 2026-11-12

## Context
ADR-0012 says every service "exports OTLP to an in-cluster OTel Collector, which routes metrics → Prometheus
(remote write)". ADR-0011 and the M01 Helm library chart also ship a `ServiceMonitor`, which implies Prometheus
*pulls* `/metrics`. Both cannot be the single source of metrics without double counting, and the choice shapes
`pkg/telemetry`, the collector config, the dashboards and the kind e2e check (traces/metrics of the test chart must
appear in Tempo/Prometheus while the obs stack runs in docker compose on the host).

## Options considered
1. **Pull metrics from `/metrics` (OTel SDK → Prometheus exporter) as the default; push traces over OTLP/gRPC;
   optionally push metrics over OTLP when configured.** Pull is the Kubernetes standard (ServiceMonitor, kubelet-style
   target discovery, `up` metric for free), works without a collector, and the Prometheus registry also hosts the
   stock Go/process collectors that every Grafana "Go service" dashboard expects. OTLP push covers environments where
   the scraper cannot reach the pod (kind → host compose stack, laptops).
2. Push everything over OTLP, collector remote-writes to Prometheus — single path, but no `up`/scrape health, remote
   write receiver must be enabled, metric names/labels get translated by the collector (surprises in dashboards),
   and a collector outage blanks all metrics.
3. Pull everything, including traces via a collector `prometheus` receiver — traces cannot be pulled; not viable.

## Decision
Option 1, implemented in `pkg/telemetry`:
- One `sdkmetric.MeterProvider` with a Prometheus reader (always) and a periodic OTLP reader when
  `TELEMETRY_METRICS_PUSH=true` and `OTEL_EXPORTER_OTLP_ENDPOINT` is set (spec-defined names keep their OTel spelling; InfraPilot-specific ones use `TELEMETRY_`). The Prometheus registry also registers
  `collectors.NewGoCollector()` and `NewProcessCollector()` so standard `go_*`/`process_*` series exist.
- One `sdktrace.TracerProvider` with parent-based ratio sampling; batch OTLP/gRPC exporter when the endpoint is set.
- Logs are JSON on stdout with `trace_id`/`span_id` (pkg/log); the compose/k8s log pipeline ships them to Loki.
- Resource attributes: `service.name`, `service.version`, `service.instance.id` (hostname/pod), `deployment.environment.name`.
- The collector config in `observability/` accepts OTLP (traces + metrics), remote-writes metrics to Prometheus
  (`--web.enable-remote-write-receiver`) and sends traces to Tempo. In Kubernetes, Prometheus scrapes via
  `ServiceMonitor`; in compose and kind-to-host runs, services push with `TELEMETRY_METRICS_PUSH=true`.

## Consequences
- Good: metrics survive a collector outage; `up` and scrape duration are available; dashboards use untranslated names;
  the same binary works in compose, kind and prod via configuration only.
- Bad / trade-offs we accept: when push is enabled in an environment that also scrapes, the same series exist twice
  with different job labels — the environment, not the service, owns the choice; runtime metrics appear twice under
  different naming conventions (`go_*` from client_golang, `go.*`/`process.runtime.*` from OTel runtime instrumentation).
- Verification: `pkg/telemetry` tests (exposition content, OTLP export to an in-process fake collector, shutdown
  bounded by context); M01 e2e asserts the conformance service's traces in Tempo and metrics in Prometheus.

## Interview one-liner
Pull for metrics because Kubernetes already solved discovery and liveness for scrapes; push for traces because
there is nothing to pull — and the SDK lets one binary do either, so the environment decides.
