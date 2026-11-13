# ADR-0022: RBAC model, API-key scopes and where authorization is enforced

- Status: accepted (refines ADR-0009) · Decider: claude-senior · Date: 2026-11-13

## Context
security.md names four org roles (`owner`, `admin`, `operator`, `viewer`), API-key scopes (`ingest`, `read`, `write`)
and a `control-plane.CheckPermission` RPC "cached 30 s in Redis". ADR-0009 requires role changes to "apply
immediately" because roles live in control-plane, not in tokens. Nothing defines which role may do what, how API
keys map onto the same model, or how a 30 s cache coexists with immediate effect. M02 must ship an RBAC matrix
test for every role, so the matrix has to be written down first.

## Options considered
1. **Gateway-only enforcement** with the cached `CheckPermission` — one enforcement point, but revocations take up
   to 30 s everywhere and every service trusts the gateway blindly.
2. **Owning-service-only enforcement** — immediate and authoritative, but each later service (check-runner,
   incident-engine, …) would need its own copy of membership data or a synchronous call per request.
3. **Both, with distinct roles:** the owning service is authoritative for its own data and checks inside the same
   transaction that reads/writes; the gateway pre-authorizes every org-scoped route through `CheckPermission`
   (cached) so services that have no membership data can rely on it, and so most denials stop at the edge.

## Decision
Option 3.

**Permissions** (stable strings; the role table lives in control-plane `internal/domain`, pure and unit-tested):

| Permission | owner | admin | operator | viewer |
|---|:-:|:-:|:-:|:-:|
| `org.read`, `member.read`, `project.read`, `resource.read` | ✓ | ✓ | ✓ | ✓ |
| `resource.write` (create, update, delete) | ✓ | ✓ | ✓ | |
| `project.write` (create, update, delete) | ✓ | ✓ | | |
| `member.manage` (add, change role, remove) | ✓ | ✓¹ | | |
| `apikey.read`, `apikey.manage` | ✓ | ✓ | | |
| `audit.read` | ✓ | ✓ | | |
| `org.update` | ✓ | ✓ | | |
| `org.delete` | ✓ | | | |

¹ Invariants enforced by control-plane in the membership transaction (row locked `FOR UPDATE`): only an owner can
grant, change or revoke the `owner` role; an admin cannot modify or remove an owner; the last owner of an
organization cannot be removed or demoted (`FAILED_PRECONDITION` → 409). Members may remove themselves unless they are
the last owner. Later milestones add permissions (checks, incidents, workflows) to this table; roles stay four.

**API keys** are project-scoped, created by owner/admin, and never carry membership, key or org-management rights:
`read` → `org.read`, `project.read`, `resource.read` limited to the key's project; `write` → `read` plus
`resource.write` in that project; `ingest` → telemetry ingestion only (M06), no control-plane permissions.
Format: `ipk_` + 43 chars base64url (32 random bytes); stored as SHA-256 (high-entropy secret, so an unsalted fast
hash is appropriate and allows an indexed lookup); a non-secret display prefix (`ipk_` + 8 chars) is stored for UIs;
the full key is returned once. Keys may carry an expiry; revocation is a timestamp.

**Enforcement:**
- control-plane checks the permission inside each RPC's transaction from its own tables → role changes and key
  revocations take effect on the next control-plane call (ADR-0009 "immediately").
- The gateway calls `CheckPermission(principal, org, permission, project?)` for every org-scoped route and caches
  the decision in Redis (allow 30 s, deny 5 s); on Redis failure it calls control-plane directly (F03). For
  services without membership data (M03+), the staleness bound for a revoked member is therefore 30 s; this is the
  accepted trade-off of security.md §1.
- Resource lookups across tenants return `NOT_FOUND`, never `PERMISSION_DENIED`, so IDs from another org reveal
  nothing.

**Adding members.** Owners/admins add users who have signed in at least once, by **verified** e-mail address
(the identity provider's `email_verified` claim, stored per user; unverified addresses never match). This lets an
organization admin learn whether an address has an account and attach that user without an acceptance step;
accepted for M02 with these mitigations: verified addresses only, a per-user rate limit on membership changes at
the gateway (ADR-0026), every addition audited, and members may leave at any time. An invitation flow (pending
invite accepted by the invitee) replaces direct adds when the dashboard ships (M07).

**Who may call `CheckPermission`.** Only the edge in M02. A principal is accepted from edge peers only (ADR-0025),
so internal services that need decisions (M03+) come with an ADR that defines how they obtain a principal.
Whenever a decision names a project, the project must be a live project of the named organization, whatever the
principal kind, so a cached decision can never cover another tenant's project.

## Consequences
- Good: one written matrix drives the domain table, the gRPC matrix test and the API matrix test; owner lockout is
  impossible; API keys cannot escalate.
- Bad / trade-offs we accept: two permission checks on control-plane routes (the gateway one is usually a cache hit);
  up to 30 s revocation lag for non-owning services and for cached API-key lookups at the gateway.
- Verification: `TestPermissions_Matrix` (domain golden table), gRPC and API RBAC matrix tests for every role and
  key scope, last-owner and admin-vs-owner tests, revocation-is-immediate test at control-plane.

## Interview one-liner
The service that owns the data is the authority and checks inside its own transaction; the edge check is a cached
optimization, so a cache can make a denial faster but never makes a revoked user's write succeed on that service.
