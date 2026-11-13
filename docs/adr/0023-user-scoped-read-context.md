# ADR-0023: User-scoped read context (`app.user_id`) for "my organizations"

- Status: accepted (extends ADR-0018) · Decider: claude-senior · Date: 2026-11-13

## Context
`GET /v1/organizations` lists the organizations the caller belongs to, with names and the caller's role. Under
ADR-0018 every tenant read runs in `WithTx(org)`, which needs the org first. ADR-0018 sketched a non-tenant lookup
table (`user_org_memberships`) for this, but the response needs organization names — tenant payload that lookup
tables may not hold — so the lookup alone cannot serve the page, and fetching each org in its own transaction is
N+1 round trips per page. ADR-0018 anticipated this: "a second policy dimension (`app.user_id`, `WithUserTx`) is
added by a new ADR".

## Options considered
1. Lookup table with ids only + one `WithTx(org)` per listed org — no policy change, but up to `limit` (200)
   transactions per page; p99 target (300 ms) at risk.
2. Denormalize org names into the lookup table — violates ADR-0018 (tenant payload outside RLS) and needs sync.
3. **Second RLS dimension:** `pkg/postgres.WithUserTx(ctx, userID, fn)` sets `app.user_id` (and clears `app.org_id`)
   in a read-only transaction; `memberships` gets a SELECT-only permissive policy `user_id = app.user_id`, and
   `organizations` a SELECT-only permissive policy "exists a membership of `app.user_id` in this org" (the
   subquery itself runs under the `memberships` policies). One indexed query per page.

## Decision
Option 3. The lookup table of ADR-0018's example becomes unnecessary: memberships already relate users to orgs.
- Policies use `NULLIF(current_setting('app.user_id', true), '')::uuid`, so an unset value matches nothing.
- The user policies are `FOR SELECT` only; writes still need `app.org_id` via `WITH CHECK`, and `WithUserTx` is
  forced `READ ONLY`.
- `WithTx` and `WithSystemTx` clear `app.user_id` like they clear `app.org_id`, so a session-level value can never
  apply.
- Only the tables that need it get a user policy (`memberships`, `organizations`); adding one elsewhere needs an ADR.

## Consequences
- Good: one query per page; no denormalized payload; fail-closed semantics preserved.
- Bad / trade-offs we accept: with the `OR` in the policy, "my organizations" sorts the user's memberships before
  applying the page limit — fine for the handful of organizations a user belongs to, revisit if that grows; the `OR` of two permissive policies is part of every query on those two tables; org-scoped
  queries keep their explicit `organization_id = $1` predicate so the index is used (verified by EXPLAIN); one extra
  `set_config` per transaction.
- Verification: `pkg/postgres` tests (user context unset → no rows; cannot write; cleared by `WithTx`), control-plane
  cross-tenant tests (a user sees only orgs they belong to, and no other members), EXPLAIN evidence for both paths.

## Interview one-liner
Instead of a lookup table that would have to copy tenant data, the database answers "which orgs is this user in"
under a second, read-only RLS dimension that fails closed exactly like the tenant one.
