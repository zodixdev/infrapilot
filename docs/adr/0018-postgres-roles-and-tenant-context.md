# ADR-0018: Three PostgreSQL roles per service and transaction-scoped tenant context

- Status: accepted (refines ADR-0008 and ADR-0010) · Decider: claude-senior · Date: 2026-11-12

## Context
ADR-0010 relies on Row-Level Security as the second line of tenant isolation and states that "service DB roles
are not table owners and do not bypass RLS". Table owners and superusers bypass RLS unless `FORCE ROW LEVEL
SECURITY` is set, so the role that runs migrations (DDL, owner) cannot be the role the service uses at runtime.
ADR-0010 also requires "a separate, audited DB role" for background jobs that legitimately span tenants. No
document defines these roles, who creates them, how the Helm migration Job authenticates, or how the tenant
id reaches the policy. `pkg/postgres.WithTx` (M01) has to encode these rules.

## Options considered
1. One role per service that owns the tables, `FORCE ROW LEVEL SECURITY` on every table — a single credential
   and simpler provisioning, but one forgotten `FORCE` silently disables isolation, and the runtime credential can
   run DDL (`DROP TABLE`) — the blast radius of a leaked secret is the whole database.
2. **Three roles per service** — `<svc>_migrator` (owner, DDL only, used by the Helm pre-upgrade Job),
   `<svc>_app` (runtime: DML on granted tables, `NOBYPASSRLS`, no DDL), `<svc>_batch` (cross-tenant jobs:
   `BYPASSRLS`, used only by named jobs, its connections tagged with `application_name` for audit).
   More provisioning, but least privilege and RLS cannot be bypassed by the runtime credential by construction.
3. Two roles (owner + app) and run cross-tenant jobs as the owner — fewer roles, but the owner credential then
   lives in a long-running process and it can run DDL.

## Decision
Option 2.
- Roles are created once per database by the platform operator (local: compose init script; prod: CloudNativePG
  managed roles / External Secrets). Migrations run as `<svc>_migrator` and must `GRANT SELECT, INSERT, UPDATE,
  DELETE` on each new table to `<svc>_app` (and sequences `USAGE`). A repository test fails if a tenant table lacks
  RLS or the grant.
- Every tenant table has `organization_id UUID NOT NULL`, `ENABLE ROW LEVEL SECURITY` and a policy
  `USING (organization_id = NULLIF(current_setting('app.org_id', true), '')::uuid)` (also `WITH CHECK`). The `true` argument
  returns `NULL` for a never-set parameter and `NULLIF` maps the empty string Postgres returns once a local setting has been reset; either way the comparison yields `NULL`, which matches no row — a transaction that forgot to set the tenant
  sees and writes nothing instead of everything.
- The tenant context is set per transaction, never per session, so pooled connections cannot leak it:
  `pkg/postgres.WithTx(ctx, orgID, fn)` runs `SELECT set_config('app.org_id', $1, true)` right after `BEGIN`
  (the `is_local = true` form of `SET LOCAL`, parameterizable). `WithSystemTx` sets nothing and is for tables
  that are not tenant data (outbox, processed_messages, migrations bookkeeping); under `<svc>_app` it cannot
  reach tenant rows because of the `NULL` rule above.
- `WithTx` rolls back on error and on panic (and re-panics, so the transport recovery middleware reports it),
  commits otherwise, and applies the pool's statement/lock/idle-in-transaction timeouts to every connection.
- Infra tables (`outbox`, `processed_messages`) are service-internal, carry no RLS, and are reached only through
  `WithSystemTx`; they are not "tenant tables" in the sense of AGENTS.md §7.

## Consequences
- Good: a leaked runtime credential cannot alter schema or bypass RLS; forgetting the tenant context fails closed;
  cross-tenant work is explicit (`_batch` role) and attributable via `application_name`.
- Bad / trade-offs we accept: three secrets per service to provision and rotate; `set_config` adds one
  round trip per transaction (pipelined with `BEGIN` by pgx, measured negligible against query cost).
- Verification: `pkg/postgres` integration tests (M01) — app role cannot read another tenant's rows, a system
  transaction sees no tenant rows, the setting is gone after commit on the same pooled connection, rollback on
  error and panic; per-service cross-tenant suites from M02 on.

## Interview one-liner
Owners bypass RLS, so the role that runs migrations must never be the role that serves requests — and setting
the tenant per transaction instead of per session is what keeps a connection pool from leaking it.
