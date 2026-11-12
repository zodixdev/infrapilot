# Runbook — shared platform (pkg/, Helm library, observability)

Applies to every Go service built on `pkg/` and deployed with `infrapilot-lib`. Service-specific
runbooks (`docs/runbooks/<service>.md`) link here for the shared parts.

## Where to look

| Need | Place |
|---|---|
| Dashboard | Grafana → *InfraPilot / Go service* (`infrapilot-go-service`), pick `job` |
| Logs | Loki, `{service_name="<svc>"}`; every line has `trace_id`, `request_id`, `org_id` when known |
| Traces | Tempo, search `service.name=<svc>`; HTTP spans are named `METHOD /route/{pattern}` |
| Readiness document | `kubectl -n <ns> exec deploy/<svc> -- wget -qO- localhost:8081/readyz` (JSON per dependency) |
| Metrics | admin port `:8081/metrics` (Prometheus exposition) |
| Profiles | `ADMIN_PPROF=true` → `:8081/debug/pprof/` (never exposed through the ingress) |

Admin endpoints live on their own port (`ADMIN_ADDR`, default `:8081`) and are reachable only from
Prometheus scrapers and kubelet probes per the NetworkPolicy.

## Alerts

### ServiceTargetDown
**Meaning:** Prometheus cannot scrape the pod (or the OTLP push stopped) for 5 minutes.
**Check:** `kubectl -n <ns> get pods -l app.kubernetes.io/name=<svc>`; `kubectl describe pod` for OOMKilled / CrashLoopBackOff;
`kubectl logs --previous`. If pods are healthy, check the NetworkPolicy `metricsScrapers` selector matches the Prometheus namespace.
**Mitigate:** roll back the release (`helm rollback`) if it started with a deploy; raise memory limits if OOMKilled.

### ServiceNotReady
**Meaning:** `health_ready == 0` for 5 minutes — the readiness gate is closed. Either startup never
completed, a dependency check is failing (see `DependencyCheckFailing`), or shutdown is stuck.
**Check:** readiness document (`/readyz`) names the failing check with its error; logs contain `health check transition`.
**Mitigate:** fix the dependency; if the process hangs in shutdown longer than `SHUTDOWN_TIMEOUT`, the kubelet kills it at
`terminationGracePeriodSeconds` — look for `components did not drain in time` in logs and open a bug with the component name.

### DependencyCheckFailing
**Meaning:** `health_check_up{check="<name>"} == 0` for 3 minutes. Checks are registered per dependency (e.g. `postgres`).
**Check:** the dependency itself (Postgres pods/CNPG cluster, broker); connection limits (`PgPoolSaturated`); credentials
rotated without a rollout (auth errors in logs — passwords are redacted, the error text is not).
**Mitigate:** restore the dependency; readiness recovers automatically within one probe interval.

### HttpHighErrorRate / GrpcHighErrorRate
**Meaning:** > 5 % of responses are 5xx (HTTP) or `Internal|Unknown|Unavailable|DataLoss` (gRPC) over 5 minutes.
**Check:** dashboard "Errors / s by status" and "Non-OK codes"; Tempo search `status=error service.name=<svc>`;
logs at `WARN` with `http request` / `grpc request` and the route. Panics are logged as `panic in http handler` with a stack.
**Mitigate:** roll back if correlated with a deploy; if a downstream is the cause, its own alert should be firing — follow it.

### HttpHighLatencyP99
**Meaning:** p99 above the 300 ms public-API SLO for 10 minutes.
**Check:** latency panel per route; Postgres pool waits (`PgPoolWaiting`); GC pause and CPU throttling panels;
`statement_timeout` errors (`SQLSTATE 57014`) in logs.
**Mitigate:** scale out (HPA max), raise pool size only if Postgres has headroom, capture `/debug/pprof/profile?seconds=30`
and attach it to the incident.

### GoroutineGrowth
**Meaning:** goroutines above 5000 and rising for 30 minutes — likely a leak (missing cancellation, blocked channel).
**Check:** `/debug/pprof/goroutine?debug=2` — group by the top frame; compare two snapshots 5 minutes apart.
**Mitigate:** restart the pod to relieve pressure, keep the dumps, open a bug. Every goroutine in `pkg/` has an owner and
a cancellation path; a leak in `pkg/` is a blocker.

### PgPoolSaturated / PgPoolWaiting
**Meaning:** more than 90 % of pool connections are acquired, or requests wait for a connection.
**Check:** `pgxpool_*` panel; slow queries (`pg_stat_statements`, `log_min_duration_statement`); long transactions
(`idle_in_transaction_session_timeout` is 60 s by default and will kill them).
**Mitigate:** fix the slow query or missing index first (`EXPLAIN (ANALYZE, BUFFERS)`); raising `MaxConns` across N
replicas must stay below Postgres `max_connections` minus reserved connections.

## Graceful shutdown sequence (what a rollout does)

1. Kubernetes sends `SIGTERM`. `pkg/app` flips the readiness gate off: `/readyz` → 503, gRPC health → `NOT_SERVING`.
2. The process keeps serving for `SHUTDOWN_DELAY` (default 5 s) so endpoints are removed from Services/load balancers.
3. Components stop in reverse start order within `SHUTDOWN_TIMEOUT` (default 25 s): HTTP/gRPC listeners close and drain
   in-flight requests, background loops stop, then pools and telemetry flush.
4. `terminationGracePeriodSeconds` = delay + timeout + 5, so the kubelet never kills a draining pod early.
A request that must outlive a rollout is a bug; the client should retry idempotent calls with backoff.

## Tenant context (Postgres)

Every tenant-scoped query runs inside `WithTx(ctx, orgID, ...)`, which sets `app.org_id` for the transaction only.
If a service returns empty results for a tenant that clearly has data, the usual cause is a code path using
`WithSystemTx` (no tenant) or a query outside `WithTx` — RLS fails closed by design (ADR-0018).

## Local reproduction

```
make up PROFILE=core,obs          # dependencies + Grafana on :3000 (admin password in .env)
make cluster deploy               # conformance release in kind, helm test
make e2e                          # traces in Tempo, metrics in Prometheus
```
