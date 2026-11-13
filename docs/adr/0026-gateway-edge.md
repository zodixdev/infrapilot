# ADR-0026: Gateway edge mechanics — JWKS cache, API-key and permission caches, rate limiting

- Status: accepted · Decider: claude-senior · Date: 2026-11-13

## Context
The gateway authenticates every public request and protects the platform. Failure requirements: F02 (Keycloak down:
existing tokens keep validating, new logins fail gracefully) and F03 (Redis down: rate limiting fails **open** with a
metric + alert; permission cache falls back to gRPC). Public read p99 < 300 ms.

## Options considered
JWT: (a) `coreos/go-oidc` — needs discovery at startup and refetches the JWKS on every unknown `kid` without a rate
limit (a stream of forged `kid`s becomes load on Keycloak); (b) `lestrrat-go/jwx` — complete but large; (c) **a small
JWKS cache on `go-jose/v4`**, which we need anyway for verification. Rate limiting: fixed window (bursty at window
edges), sliding log (memory per request), **GCRA** (one key per principal, O(1), smooth, exact `Retry-After`);
local token buckets per replica (limits multiply with replicas).

## Decision
- **JWT:** gateway verifies locally with go-jose: algorithm allow-list RS256/ES256 (no `none`/HMAC), `iss` exact,
  `aud` contains `infrapilot-api`, `exp`/`nbf` with 30 s leeway, `typ` = `Bearer` (Keycloak access tokens), `sub`
  required. JWKS fetched from a configured URL (no discovery dependency): refreshed every 10 min in the background,
  on an unknown `kid` at most once per 30 s (singleflight), keys kept and served when refreshes fail (F02). Startup
  does not require Keycloak; JWT requests fail with 503 `unavailable` until the first JWKS load, API-key requests
  work. Metrics: refresh outcome counter, key-set age gauge; alert when age > 1 h.
- **API keys:** `X-API-Key` → SHA-256 → Redis cache (30 s, keyed by hash, value = resolved principal, negative 5 s) →
  `AuthenticateAPIKey` gRPC on miss or Redis failure.
- **Permissions:** `CheckPermission` decisions cached in Redis (allow 30 s, deny 5 s) keyed by
  `(principal, org, permission, project)`; Redis failure → direct gRPC (F03).
- **Rate limiting:** GCRA in one Redis Lua script (`EVALSHA`, atomic), keys per principal: user, API key, or
  client IP for unauthenticated requests (client IP from `X-Forwarded-For` only when the peer is in configured
  trusted proxy CIDRs). Defaults: user 50 rps burst 100, API key 100 rps burst 200, anonymous 10 rps burst 20 (Helm
  values). Responses carry `RateLimit-Limit`/`RateLimit-Remaining`; 429 adds `Retry-After`. Redis calls have a 50 ms
  deadline; a circuit breaker opens after 5 consecutive failures for 5 s so an outage does not add the deadline to
  every request. While Redis is unavailable requests are **allowed**, counted in
  `gateway_ratelimit_bypassed_total{reason}`, and `GatewayRateLimitFailOpen` fires.
- Caches never store raw keys or tokens; Redis keys use hashes (length-prefixed parts). Cached values are
  authenticated with HMAC-SHA256 over (key, value) using a gateway-only secret (`CACHE_SIGNING_KEY`), so write
  access to Redis is not enough to plant a principal or a decision; a failed check counts as a miss. Redis requires
  authentication and TLS in staging and prod. Concurrent misses for the same key share one upstream call.
- Tokens are additionally bound to the API's OAuth clients (`azp` allow-list) and to a maximum lifetime (`exp - iat`
  ≤ 1 h).
- **Failed authentication budget:** before resolving any credential, a per-client-IP GCRA bucket of failed
  authentications is checked without consuming it; every failure consumes from it (default 1/s, burst 20). Credential
  guessing is therefore refused before signature checks or control-plane lookups happen. Like all limits it fails open
  without Redis. Check and charge are separate round trips, so a client can exceed the budget by its number of
  concurrent in-flight requests — accepted: still bounded, and each attempt is a cheap edge operation. Clients
  behind one NAT share the budget, so one misbehaving client can get its neighbours 429 for failed attempts only
  (successful requests never consume it) — accepted over a budget keyed by the presented credential, which a guesser
  rotates freely.
- GCRA uses the Redis server clock (`TIME`), so all gateway replicas agree; after a Redis failover to a node with a
  skewed clock, limits are briefly stricter or looser by the skew — accepted (limits are protective, not billing).
- Per-principal buckets are per user (not per user and organization: list routes have no organization). Sensitive
  operations (member changes, organization and API-key creation) have additional per-principal buckets, keyed by the
  generated operation name; a unit test keeps the edge policy table and the contract in sync.

## Consequences
- Good: Keycloak and Redis outages degrade gracefully as specified; one Redis round trip for limiting; no JWKS storm.
- Bad / trade-offs we accept: revoked API keys and permissions may be honored by the gateway for up to 30 s (owning
  services check authoritatively, ADR-0022); during a Redis outage there is no rate limiting at all (by requirement) —
  per-replica in-memory concurrency limits in `pkg/httpserver` (body size, timeouts) remain.
- Verification: F02 and F03 integration tests against real Keycloak and Redis containers stopped mid-test; GCRA test
  under concurrency; JWKS hit counting for random `kid`s.

## Interview one-liner
Authentication must survive the identity provider being down, so keys are cached and served stale; rate limiting
must not take the API down with Redis, so it fails open loudly — and both behaviors are tests, not hopes.
