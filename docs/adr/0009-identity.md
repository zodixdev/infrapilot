# ADR-0009: Keycloak for users, internal CA + mTLS for agents

- Status: accepted · Decider: senior-backend · Date: 2026-11-12

## Decision
- Users: Keycloak realm `infrapilot`, OIDC Authorization Code + PKCE. Gateway validates JWTs locally
  (JWKS cached, refreshed on unknown `kid`). Org roles are stored in control-plane, **not** in token claims,
  so role changes apply immediately.
- API keys: project-scoped, hashed, scoped, revocable (control-plane).
- Agents: enrollment token (single-use, 1 h TTL) → agent generates key pair → CSR → agent-manager CA signs
  a 24 h cert with org/agent IDs in the SAN URI. Auto-rotation; revocation list checked on connect.
- Services: cert-manager-issued TLS certs; peer allow-list per RPC.

## Alternatives rejected
Building our own user auth (security risk, no value). Long-lived agent API keys (no rotation, easy to leak).
Service mesh (Linkerd/Istio) — postponed: more moving parts than needed while cert-manager covers TLS.
