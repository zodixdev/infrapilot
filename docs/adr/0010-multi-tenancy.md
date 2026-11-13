# ADR-0010: Shared schema + organization_id + Row-Level Security

- Status: accepted · Decider: senior-backend · Date: 2026-11-12

## Options
1. Database per tenant — strong isolation, unmanageable at 1,000+ orgs.
2. Schema per tenant — migrations × tenants, connection/catalog bloat.
3. **Shared schema, `organization_id` on every tenant row, enforced in code and by Postgres RLS.**

## Decision
Option 3. Two independent layers: repository methods require a tenant ID; RLS policies
`USING (organization_id = current_setting('app.org_id')::uuid)` block anything that slips through.
(The exact policy expression, role model and schema rules are refined in ADR-0018.)
Per-tenant quotas (checks, agents, ingest rate, retention) prevent noisy neighbors.

## Consequences
Cross-tenant tests are mandatory for every service. Background jobs that legitimately span tenants
use a separate, audited DB role.
