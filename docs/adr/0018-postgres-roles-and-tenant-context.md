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
- Roles are created once per database by the platform operator (local: idempotent compose init script /
  `make db-provision`; prod: CloudNativePG managed roles / External Secrets). Provisioning revokes all PUBLIC
  privileges on the database (CONNECT, TEMP) and on schema `public`, grants CONNECT to the three roles only, and sets
  **server-side timeouts as role defaults** (`ALTER ROLE … SET`): `_app` statement 30 s / lock 5 s / idle-in-tx 60 s,
  `_batch` statement 15 min / idle-in-tx 5 min, `_migrator` lock 3 s. Client-side values are overrides only, so the
  guards also hold behind PgBouncer and for ad-hoc sessions.
- No default privileges are set at provisioning. Every migration grants its tables explicitly (`_app`: the DML it
  needs; append-only tables such as audit logs get `SELECT, INSERT` only; `_batch`: per table, e.g. `SELECT, DELETE`
  for retention purges), so compose, tests and prod behave identically.
- `pgtest.AssertTenantSchema` (M01) runs in every service's integration suite after migrations and fails when a
  tenant table (one with `organization_id`) lacks RLS, a policy on `app.org_id`, or the `_app` grant; when a
  non-primary UNIQUE index or a foreign key on a tenant table omits `organization_id` (a duplicate-key or FK error
  would reveal another tenant's rows — primary keys are exempt because they are server-generated surrogates);
  when a view lacks `security_invoker = true` (views owned by the migrator bypass RLS); or when a SECURITY DEFINER
  function is not on the service's reviewed allow-list (such functions must also `SET search_path`).
- Every tenant table has `organization_id UUID NOT NULL`, `ENABLE ROW LEVEL SECURITY` and a policy
  `USING (organization_id = NULLIF(current_setting('app.org_id', true), '')::uuid)` (also `WITH CHECK`). The `true` argument
  returns `NULL` for a never-set parameter and `NULLIF` maps the empty string Postgres returns once a local setting
  has been reset; either way the comparison yields `NULL`, which matches no row — a transaction that forgot to set
  the tenant sees and writes nothing instead of everything. The expression is sargable: verified as an `Index Cond`
  on `(organization_id, …)` indexes in `EXPLAIN (ANALYZE, BUFFERS)`.
- The tenant context is set per transaction, never per session, so pooled connections cannot leak it:
  `pkg/postgres.WithTx(ctx, orgID, fn)` runs `SELECT set_config('app.org_id', $1, true)` right after `BEGIN`
  (the `is_local = true` form of `SET LOCAL`, parameterizable). `WithSystemTx` explicitly sets it to `''` and is for
  tables that are not tenant data (outbox, processed_messages, pre-tenant lookups below); under `<svc>_app` it cannot
  reach tenant rows, even if something left a session-level value on the pooled connection. The underlying pgx
  pool is not exported, so no code path can issue `SET app.org_id` outside these two functions.
- `WithTx` on a pool whose role bypasses RLS (`_batch`, detected at `Open`) returns `ErrBypassRLS`: the tenant
  context would be silently ignored. Batch jobs use `WithSystemTx` and filter explicitly; `application_name`
  identifies the job, and prod enables `log_statement = 'mod'` for `_batch` roles (application_name is
  client-controlled and therefore not an audit control on its own).
- `WithTx` rolls back on error and while a panic unwinds (the panic continues with its original stack), with a
  bounded rollback timeout; pgx discards connections whose rollback fails, so no dirty connection returns to the pool.
- **Migrations** use `postgres.MigrationConfig`: one connection, no statement timeout (backfills,
  `CREATE INDEX CONCURRENTLY`, which must run outside a transaction via goose `NO TRANSACTION`), lock timeout 3 s with
  retry on SQLSTATE 55P03, statement text omitted from spans. Concurrent hook jobs serialize on goose's advisory lock.
- **Pre-tenant lookups** (needed from M02): some reads happen before the tenant is known — resolving an API key by
  hash, mapping an OIDC subject to the user's memberships across orgs, `CheckPermission(user, org)`. They use narrow,
  **non-tenant lookup tables** reached through `WithSystemTx` (e.g. `api_key_index(key_hash PK, organization_id,
  key_id, revoked_at)`, `user_org_memberships(subject, organization_id, role)`), declared non-tenant to
  `AssertTenantSchema`; the request then continues in `WithTx(org)`. Lookup tables hold only the identifiers needed
  to choose the tenant, never tenant payload. Running such reads on the `_batch` pool or disabling RLS ad hoc is
  forbidden. If user-scoped rows are needed later, a second policy dimension (`app.user_id`, `WithUserTx`) is added
  by a new ADR. The `organizations` table itself is a tenant table keyed on `id = app.org_id`; creating an org runs
  in `WithTx(newOrgID)`.
- Infra tables (`outbox`, `processed_messages`) are service-internal, carry no RLS, and are reached only through
  `WithSystemTx`; they are not "tenant tables" in the sense of AGENTS.md §7.

## Consequences
- Good: a leaked runtime credential cannot alter schema or bypass RLS; forgetting the tenant context fails closed;
  cross-tenant work is explicit (`_batch` role) and attributable via `application_name`.
- Bad / trade-offs we accept: three secrets per service to provision and rotate; `set_config` adds one round trip
  per transaction (pgx sends `BEGIN` synchronously, so it is not pipelined); if it ever matters, `BEGIN` and the
  `set_config` can be sent as one simple-protocol batch.
- Verification: `pkg/postgres` integration tests (M01) — app role cannot read or write another tenant's rows, a
  system transaction sees no tenant rows even after a session-level `SET`, `WithTx` refuses BYPASSRLS pools, role
  defaults apply without startup parameters, PUBLIC CONNECT is revoked, the migration profile disables statement
  timeouts, the schema rules catch views/unique indexes/definers, rollback on error and panic; per-service
  cross-tenant suites from M02 on.

## Interview one-liner
Owners bypass RLS, so the role that runs migrations must never be the role that serves requests — and setting
the tenant per transaction instead of per session is what keeps a connection pool from leaking it.
