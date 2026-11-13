# Runbook — api-gateway

The public REST edge (ADR-0003, ADR-0026): verifies Keycloak tokens against a cached JWKS, resolves API keys through
control-plane (cached in Redis), rate-limits with GCRA in Redis, validates every request against
`contracts/openapi/public.yaml`, pre-authorizes org-scoped routes with `CheckPermission` (cached), and forwards to
internal services over mTLS gRPC. It holds no business data; Redis holds only rebuildable caches and counters.

## Where to look

- Dashboard **InfraPilot / API gateway** (`observability/grafana/dashboards/api-gateway.json`): requests, errors and
  latency by route, 401/403/429 rates, cache hit ratio, Redis breaker, JWKS age, upstream gRPC errors.
- Generic dashboard **InfraPilot / Go service** (`job=api-gateway`) for runtime and HTTP RED.
- Logs: JSON with `request_id` (minted by the gateway; a client-supplied one is logged as `client_request_id`) and
  `trace_id`. Problem responses carry `trace_id` — search Tempo with it.
- `/readyz` on the admin port is process-level only: Keycloak, Redis or control-plane outages do **not** make the
  gateway unready (that would turn a dependency outage into a total outage); they show as alerts below.

## Degraded modes (by design)

| Dependency down | Behavior |
|---|---|
| Keycloak (F02) | Tokens issued before keep validating with the cached keys. A gateway that never loaded keys answers 503 `unavailable` for tokens; API keys keep working. New logins fail at Keycloak. |
| Redis (F03) | Rate limiting is **off** (fail open, `gateway_ratelimit_bypassed_total`); API keys and permissions are resolved through control-plane per request (higher latency, more control-plane load). A circuit breaker keeps Redis timeouts off the request path. |
| control-plane | Requests needing it answer 503 `unavailable` with `Retry-After`; reads are retried by the client library, writes are not. |

## Alerts

### GatewayRateLimitFailOpen
**Meaning:** requests are admitted without a rate-limit decision because Redis is unavailable. The API is unprotected
against floods (F03).
**Check:** Redis pods / managed instance health; `gateway_redis_errors_total` by reason; NetworkPolicy egress to Redis;
credential rotation (`NOAUTH`/`WRONGPASS` in logs).
**Mitigate:** restore Redis. If abuse is ongoing during the outage, block at the ingress (per-IP limits there) — the
gateway cannot limit without Redis by design.

### GatewayRedisBreakerOpen
**Meaning:** five consecutive Redis calls failed on an instance; Redis is skipped for 5 s at a time, one probe call
re-tests it.
**Check:** as GatewayRateLimitFailOpen; also Redis latency (each call has a 50 ms deadline — a slow Redis trips the
breaker too).
**Mitigate:** restore Redis or its latency; the breaker closes by itself after the first successful probe.

### GatewaySigningKeysStale
**Meaning:** the cached signing keys were last fetched over an hour ago; Keycloak's JWKS endpoint is unreachable from
the gateway. Existing tokens still validate; a key rotation at Keycloak would not be seen.
**Check:** `gateway_jwks_refresh_total{result="error"}`, gateway logs `JWKS refresh failed`, Keycloak health, the
`OIDC_JWKS_URL`, egress NetworkPolicy to Keycloak.
**Mitigate:** restore Keycloak or the network path. Do **not** rotate realm keys while this alert fires.

### GatewaySigningKeysUnavailable
**Meaning:** an instance has no signing keys at all (never loaded since start): every user request gets 503.
**Check:** as above, plus whether the instance started during a Keycloak outage.
**Mitigate:** restore Keycloak; the gateway retries the first load every 30 s. API-key traffic is unaffected.

### GatewayUpstreamUnavailable
**Meaning:** more than 5 % of gRPC calls to an internal service fail with UNAVAILABLE / DEADLINE_EXCEEDED; clients get 503.
**Check:** the upstream's dashboard and alerts (control-plane: Postgres, pods); `grpc_client_handled_total` by
code; certificate problems (`verify server certificate`, `server identity is not` in gateway logs — see
`TLSCertificateExpiringSoon` in the platform runbook); NetworkPolicy changes.
**Mitigate:** fix the upstream; roll back a release that changed certificates or identities.

### GatewayAuthFailureSpike
**Meaning:** more than 5 requests/s are answered 401 for 10 minutes: a broken client, an expired credential in
automation, or credential stuffing / key guessing.
**Check:** access logs grouped by `client_ip`/user agent (Loki: `{service_name="api-gateway"} | json | status="401"`);
`gateway_ratelimit_limited_total{bucket="anonymous"}`.
**Mitigate:** contact the tenant for broken automation; block abusive sources at the ingress; lower
`RATE_LIMIT_ANONYMOUS_*` (Helm values) if guessing continues.

### GatewayReadLatencyP99High
**Meaning:** GET p99 is above the 300 ms objective for 10 minutes.
**Check:** which routes (dashboard, latency by route); upstream latency (`grpc_client_handling_seconds`), cache hit
ratio (`gateway_cache_requests_total`), Redis latency, CPU throttling (HPA at max?).
**Mitigate:** scale out (HPA max), restore cache/Redis, investigate the slow upstream query (control-plane runbook).

## Operations

- **Revoking a leaked API key:** revoke it in the API (`POST …/api-keys/{id}/revoke`). Control-plane refuses it
  immediately; the edge cache may accept it for up to 30 s, but every request is re-checked by control-plane.
- **Rate-limit tuning:** `RATE_LIMIT_*` env (Helm values). Per-operation buckets exist for member changes, organization
  creation and key creation.
- **Rotating the Redis password:** update the Secret, then roll the gateway; during the gap the gateway runs in F03 mode.
