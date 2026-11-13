# Runbook — shared platform (pkg/, Helm library, observability)

Applies to every Go service built on `pkg/` and deployed with `infrapilot-lib`. Service-specific
runbooks (`docs/runbooks/<service>.md`) link here for the shared parts.

## Where to look

| Need | Place |
|---|---|
| Dashboard | Grafana → *InfraPilot / Go service* (`infrapilot-go-service`), pick `job` |
| Logs | Loki, `{service_name="<svc>"}`. Services ship logs over OTLP (`TELEMETRY_LOGS_PUSH=true`) and also write JSON to stdout (`kubectl logs`). Every line has `trace_id`, `request_id`, `org_id` when known; secrets are redacted before either destination |
| Traces | Tempo, search `service.name=<svc>`; HTTP spans are named `METHOD /route/{pattern}` |
| Readiness document | `kubectl -n <ns> exec deploy/<svc> -- wget -qO- localhost:8081/readyz` (JSON per dependency) |
| Metrics | admin port `:8081/metrics` (pull) or Prometheus via OTLP push (`TELEMETRY_METRICS_PUSH=true`); same metric names on both paths |
| Profiles | `ADMIN_PPROF=true` (+ `ADMIN_PPROF_PROD_ACK=true` in staging/prod) → `:8081/debug/pprof/`; the admin write timeout (90 s) allows `profile?seconds=60` |

Admin endpoints live on their own port (`ADMIN_ADDR`, default `:8081`) and are reachable only from
Prometheus scrapers, kubelet probes and (when `tests.enabled`) the release's own test pods.

## Alerts

### ServiceTargetDown
**Meaning:** Prometheus cannot scrape an InfraPilot pod (pull path, `up{app_kubernetes_io_part_of="infrapilot"} == 0`) for 5 minutes.
**Check:** `kubectl -n <ns> get pods -l app.kubernetes.io/name=<svc>`; `kubectl describe pod` for OOMKilled / CrashLoopBackOff;
`kubectl logs --previous`. If pods are healthy, check that the NetworkPolicy `metricsScrapers` selector matches the Prometheus namespace.
**Mitigate:** roll back the release (`helm rollback`) if it started with a deploy; raise memory limits if OOMKilled.

### ServiceTelemetryAbsent
**Meaning:** a service that pushed metrics during the last hour has not pushed for 5+ minutes (push path; there is no `up` series for pushed metrics).
**Check:** are the pods running (`kubectl get pods`)? Is the collector up (`:13133`, collector logs)? Egress NetworkPolicy to the collector?
`opentelemetry error` lines in the service logs (rate-limited, with `suppressed_repeats`).
**Mitigate:** restore the collector or the egress rule. Services keep serving without the collector; only telemetry is lost.

### ServiceNotReady
**Meaning:** `health_ready == 0` for 5 minutes — the readiness gate is closed. Either startup never completed, a dependency check
is failing (see DependencyCheckFailing), or shutdown is stuck.
**Check:** the readiness document (`/readyz`) names the failing check with its (scrubbed) error; logs contain `health check transition`.
**Mitigate:** fix the dependency; if the process hangs in shutdown longer than `SHUTDOWN_TIMEOUT`, the kubelet kills it at
`terminationGracePeriodSeconds` — look for `component did not stop in time` in logs and open a bug with the component name.
**Trade-off:** a hard dependency (Postgres) failing takes every replica out of the Service at once; clients then see connection
errors instead of 503 problem documents. This is deliberate for DB-bound services (nothing useful can be served). A saturated pool
is reported as *busy, not down* so load spikes never cause this.

### DependencyCheckFailing
**Meaning:** `health_check_up{check="<name>"} == 0` for 3 minutes. Checks are registered per dependency (e.g. `postgres`).
**Check:** the dependency itself (Postgres pods/CNPG cluster, broker); role connection limits; credentials rotated without a rollout
(auth errors in logs — passwords are redacted, the error text is not). `previous evaluation still running` means the check itself hangs.
**Mitigate:** restore the dependency; readiness recovers automatically within one probe interval.

### HttpErrorBudgetBurn
**Meaning:** the 99.9 % / 30 d availability SLO budget is being consumed too fast: *Fast* (critical) = 14.4× over 1 h and 5 m
(2 % of the monthly budget per hour), *Slow* (warning) = 6× over 6 h and 30 m. Only fires above 0.5 req/s.
**Check:** which routes return 5xx (dashboard "Errors / s by status", Tempo `status=error`), correlation with deploys.
**Mitigate:** roll back if correlated with a deploy; otherwise follow HttpHighErrorRate.

### HttpHighErrorRate
**Meaning:** > 5 % of HTTP responses are 5xx for 10 minutes (with at least 0.5 req/s). Panics count as 500.
**Check:** dashboard "Errors / s by status"; Tempo search `status=error service.name=<svc>`; logs at `WARN` with `http request` and
the route. Panics are logged as `panic in http handler` with a stack.
**Mitigate:** roll back if correlated with a deploy; if a downstream is the cause, its own alert should be firing — follow it.

### GrpcHighErrorRate
**Meaning:** > 5 % of gRPC calls end with `Internal|Unknown|Unavailable|DataLoss` for 10 minutes (with at least 0.5 calls/s).
**Check:** dashboard "Non-OK codes / s"; logs at `WARN` with `grpc request`; panics as `panic in grpc handler`.
**Mitigate:** as for HttpHighErrorRate.

### HttpHighLatencyP99
**Meaning:** p99 above the 300 ms public-API SLO for 10 minutes.
**Check:** latency panel per route; Postgres pool waits (PgPoolSaturated); GC pause and CPU panels; `statement_timeout`
errors (`SQLSTATE 57014`) in logs.
**Mitigate:** scale out (HPA max), raise pool size only within the connection budget (docs/architecture/data.md §8), capture
`/debug/pprof/profile?seconds=30` and attach it to the incident.

### GoroutineGrowth
**Meaning:** goroutines above 5000 and rising for 30 minutes — likely a leak (missing cancellation, blocked channel).
**Check:** `/debug/pprof/goroutine?debug=2` — group by the top frame; compare two snapshots 5 minutes apart.
**Mitigate:** restart the pod to relieve pressure, keep the dumps, open a bug. Every goroutine in `pkg/` has an owner and a
cancellation path; a leak in `pkg/` is a blocker.

### PgPoolSaturated
**Meaning:** more than 90 % of a pool's connections are acquired (`PgPoolSaturated`), or requests wait for a connection
(`PgPoolWaiting`). The `pool` label distinguishes the request pool from batch pools.
**Check:** `pgxpool_*` panel; slow queries (`pg_stat_statements`, `log_min_duration_statement`); long transactions
(`idle_in_transaction_session_timeout` is 60 s for the app role and will kill them).
**Mitigate:** fix the slow query or missing index first (`EXPLAIN (ANALYZE, BUFFERS)`); raising `MaxConns` across N replicas must
stay within the connection budget.

## Graceful shutdown sequence (what a rollout does)

1. Kubernetes sends `SIGTERM`. `pkg/app` flips the readiness gate off: `/readyz` → 503, gRPC health → `NOT_SERVING`.
   A second `SIGTERM`/`SIGINT` now terminates the process immediately.
2. The process keeps serving for `SHUTDOWN_DELAY` (default 5 s) so endpoints are removed from Services/load balancers.
3. Components stop in reverse start order. Each is asked to drain, then its context is cancelled and the App waits for it to
   return before stopping the next one. Listeners and workers share `SHUTDOWN_TIMEOUT` (default 25 s); pools and the telemetry
   flush share a separate 5 s close budget, so a stuck drain cannot lose the final spans and logs.
4. `terminationGracePeriodSeconds` = delay + timeout + 5, so the kubelet never kills a draining pod early.
A request that must outlive a rollout is a bug; the client should retry idempotent calls with backoff.

PodDisruptionBudgets (`maxUnavailable: 1`, unhealthy pods always evictable) are rendered only when at least two replicas are
guaranteed; a single-replica PDB would block every node drain.

## Tenant context (Postgres)

Every tenant-scoped query runs inside `WithTx(ctx, orgID, ...)`, which sets `app.org_id` for the transaction only.
`WithSystemTx` explicitly clears it. If a service returns empty results for a tenant that clearly has data, the usual cause is
a code path using `WithSystemTx` — RLS fails closed by design (ADR-0018). `ErrBypassRLS` means a batch pool was used with
`WithTx`; batch jobs must use `WithSystemTx` and filter explicitly.

## Local reproduction

```
make up PROFILE=core,obs          # dependencies + Grafana on 127.0.0.1:3000 (admin password in .env)
make cluster deploy               # conformance release in kind, helm test
make e2e                          # traces in Tempo, metrics in Prometheus, logs in Loki
make undeploy                     # remove the release and its test database
```
