# ADR-0024: `Idempotency-Key` is enforced by the owning service in the creating transaction

- Status: accepted (refines ADR-0003) · Decider: claude-senior · Date: 2026-11-13

## Context
ADR-0003 requires an `Idempotency-Key` header on POSTs that create resources; the roadmap lists it in the gateway's
scope. The guarantee clients need: retrying a create with the same key never creates a second resource and returns
the original result.

## Options considered
1. **Gateway + Redis** (lock key, forward, store response) — no service changes, but not atomic with the create:
   a crash or timeout after the create commits and before Redis stores the response lets the retry create a
   duplicate; Redis is best-effort data in our design (data.md §1) and F03 requires the API to keep working
   without it, which would silently drop the guarantee.
2. **Owning service, same transaction:** the gateway validates the header (1–128 printable ASCII) and forwards it
   in gRPC metadata; the service records `(organization_id, principal_id, key)` with a request fingerprint and the
   created resource id in the same transaction as the insert (unique constraint). A replay reads the record and
   returns the stored resource; a different fingerprint is rejected.
3. Both layers — more code, no stronger guarantee than option 2.

## Decision
Option 2.
- Table `idempotency_keys(organization_id, principal_id, key, operation, request_hash, resource_id, created_at)`,
  unique on `(organization_id, principal_id, key)`, tenant table with RLS, retention 24 h (ADR-0027).
- `request_hash` = SHA-256 over the operation name and the canonical protobuf encoding of the request.
- Concurrent duplicates: each creating transaction first claims a transaction-scoped advisory lock on
  (organization, principal, key) with `pg_try_advisory_xact_lock`, then looks the record up. A duplicate that arrives
  while the original is still running gets 409 `conflict` ("still in progress", the convention of Stripe-style
  idempotency) instead of waiting — waiting would pin one pooled connection per duplicate and let one client starve
  other tenants; retried after the original committed, it replays. (Letting duplicates race on the record's unique
  index is not enough: they could collide earlier on the resource's own unique keys, e.g. a project slug, and the
  loser would get a misleading "slug taken".)
- Within the window a record is authoritative; a record older than 24 h that the hourly purge has not removed yet is
  replaced in place.
- Mismatch → `ALREADY_EXISTS` with reason `IDEMPOTENCY_KEY_REUSED` → HTTP 422 `idempotency_key_reused`.
- Replays return the resource's **current** state with the original status code (201), not a byte-identical copy
  of the first response; if the resource (or organization) was deleted since, the replay answers 404. Creating an organization has no tenant yet: its keys live in the non-tenant table
  `org_create_keys (principal_id, key) → organization_id` (same lock, same 24 h window), written in the creating
  transaction; the organization id is a fresh UUIDv7. (An id derived from principal and key was rejected: after the
  organization is purged the same key would recreate the same id and expose the purged organization's audit history.)
- Without the header, creates are not deduplicated (documented in the OpenAPI document).

## Consequences
- Good: at-most-once effect for creates even across gateway/service crashes and Redis outages.
- Bad / trade-offs we accept: each create path in each service implements the record step (a helper moves to `pkg/`
  when a second service needs it, ADR-0016); a replay after the resource was modified returns the modified state.
- Verification: replay returns the original id; different body → 422; 20 concurrent identical requests → exactly one
  resource; replay survives a control-plane restart.

## Interview one-liner
Idempotency is only real if the dedup record commits in the same transaction as the side effect, so it lives in
the service's database, not in a cache in front of it.
