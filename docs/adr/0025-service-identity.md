# ADR-0025: Service identity, per-RPC peer allow-list and end-user principal propagation

- Status: accepted (refines ADR-0009) · Decider: claude-senior · Date: 2026-11-13

## Context
security.md §1: services authenticate with "Kubernetes ServiceAccount + TLS (cert-manager); gRPC interceptors verify
peer identity allow-list". M01 shipped TLS/mTLS in `pkg/grpcserver` but no allow-list (M01 report §9). M02 has the
first internal RPCs: api-gateway → control-plane. control-plane must know *which end user or API key* a request is
for, and must not accept that claim from anything but the edge.

## Options considered
1. Gateway forwards the user's JWT; control-plane re-validates it — every service needs JWKS handling and API keys
   need another path; tokens could be replayed by any internal caller that sees them.
2. **mTLS peer identity + a principal message in metadata accepted only from allow-listed edge peers** — one
   validation at the edge, services authorize on a typed principal, spoofing requires a gateway certificate.
3. Gateway mints a short-lived internal JWT per request (signed, audience-bound) — strongest end-to-end, but a
   signing-key lifecycle and per-request signing cost without a threat it closes that option 2 leaves open (an
   attacker with a gateway certificate can mint those too).
4. Service mesh (Linkerd/Istio) identity — postponed by ADR-0009.

## Decision
Option 2.
- **Identity:** cert-manager issues each workload a certificate from the in-cluster CA with URI SAN
  `spiffe://infrapilot.local/ns/<namespace>/sa/<serviceaccount>`; 24 h lifetime, renewed at 2/3. `pkg/grpcserver`
  reloads rotated files (M01) and requires client certs.
- **Allow-list:** `pkg/grpcserver` gains a peer-identity interceptor configured with a map from full method (or
  `service/*`) to allowed identities; unmatched methods are denied (default deny). Health and reflection are exempt.
  The allowed identities are rendered from Helm values, so they follow namespace/SA names.
- **Principal:** `infrapilot.controlplane.v1.Principal` (kind user|api_key, subject, email, display name, api key id,
  org id, project id, scopes) serialized in the binary metadata key `x-infrapilot-principal-bin`, plus
  `x-infrapilot-idempotency-key` and the request id. A control-plane interceptor accepts these only when the peer is
  in the edge set (api-gateway); otherwise the call is rejected with `PERMISSION_DENIED`.
- **Plaintext** is allowed only when `GRPC_INSECURE=true` and the environment is not `production` (config rails);
  then the peer identity is taken from a configured static value and the interceptor still runs, so tests exercise
  the same path. Integration tests use real mTLS with an in-test CA.
- `pkg/grpcclient` builds client connections: mTLS credentials with file reload, OTel stats handler, default
  per-call deadline when the caller sets none, and a service config retry policy only for RPCs marked idempotent
  (`UNAVAILABLE`, max 3 attempts, exponential backoff with jitter).

## Consequences
- Good: one authentication point for users; services receive a typed, minimal principal; a compromised non-edge
  workload cannot impersonate users; default-deny RPC surface.
- Bad / trade-offs we accept: the gateway is trusted for user identity (its compromise is total for the user plane —
  mitigated by its minimal surface and NetworkPolicies); cert-manager becomes a cluster dependency (kind included).
- Verification: interceptor unit tests (allowed, denied, wildcard, missing cert, health exempt), control-plane test
  that principal metadata from a non-edge certificate is rejected, kind e2e over mTLS.

## Interview one-liner
mTLS tells the service which workload is calling; only the edge is allowed to say which human it is calling for,
and the allow-list is default-deny per RPC.
