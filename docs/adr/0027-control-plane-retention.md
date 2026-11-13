# ADR-0027: Soft delete and retention for control-plane data

- Status: accepted · Decider: claude-senior · Date: 2026-11-13

## Context
data.md §1: control-plane data lives "until deleted (+30 d soft delete)". AGENTS.md §7: every table needs a
retention rule, no unbounded growth. M02 adds audit events and idempotency records, which grow with use.

## Options considered
1. Hard delete everywhere — simplest, but an accidental org deletion is unrecoverable and audit history disappears
   with the rows it describes.
2. **Soft delete with a bounded grace period, purged by a batch job under the `_batch` role (ADR-0018)**.
3. Partitioned tables with partition drops — efficient for very large append-only data; premature at control-plane
   volumes (thousands of rows per org).

## Decision
Option 2.
| Table | Rule |
|---|---|
| `organizations`, `projects`, `resources` | `deleted_at` set on delete; hidden from every read; unique constraints are partial (`WHERE deleted_at IS NULL`); hard-deleted 30 d later (an org purge cascades to its rows) |
| `memberships` | hard delete (the audit event preserves history) |
| `api_keys`, `api_key_index` | revoked or expired keys hard-deleted 30 d after revocation/expiry |
| `audit_events` | append-only for `_app`; deleted after 400 d; no foreign key to `organizations`, so the history survives the organization's purge |
| `idempotency_keys` | deleted after 24 h |
| `users` | kept while referenced by a membership; users without memberships and inactive for 400 d are deleted |

`control-plane purge` runs as a Kubernetes CronJob (hourly) with the `controlplane_batch` role and
`application_name=controlplane-purge`, deletes in bounded batches (`ctid IN (SELECT … LIMIT 1000)`), records
`controlplane_purge_rows_total{table}` and last success time; `ControlPlanePurgeStale` alerts after 3 h without
success. Deleting an org soft-deletes it only; its projects, resources and keys stop being reachable through the org
check (keys are revoked in the same transaction) and are hard-deleted with it: an expired organization is emptied
table by table in bounded batches before its row is deleted, so a large tenant never becomes one unbounded
cascading statement. The user purge skips rows locked by a concurrent membership insert.

## Consequences
- Good: bounded growth, recoverable deletions for 30 d (owner restore is a later feature; operators can restore by SQL
  per runbook), audit history outlives the deletion of what it describes.
- Bad / trade-offs we accept: partial indexes and `deleted_at IS NULL` predicates on every read; a deleted org's name
  and slug become reusable immediately.
- Verification: purge integration test (old rows removed, fresh rows kept, `_app` cannot delete audit rows), partial
  unique index tests.

## Interview one-liner
Soft delete buys a recovery window, the batch role keeps purges out of the request path's privileges, and every
table has a written expiry so nothing grows forever.
