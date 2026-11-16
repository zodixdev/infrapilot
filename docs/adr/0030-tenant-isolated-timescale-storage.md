# ADR-0030 — Keep TimescaleDB telemetry and rollups under tenant RLS

- Status: accepted (partially supersedes ADR-0007 for TimescaleDB)
- Date: 2026-11-16
- Decider: claude-senior

## Context

M03 requires tenant-isolated TimescaleDB storage, incremental rollups, retention and compression.
InfraPilot also requires RLS for tenant tables and service-owned database access. On the pinned
TimescaleDB 2.32.0 and PostgreSQL 17.11 image, a role with only SELECT on a two-tenant hypertable
saw zero rows with tenant context unset and one row with tenant context set. Then both
`CREATE MATERIALIZED VIEW ... WITH (timescaledb.continuous)` and
`ALTER TABLE ... SET (timescaledb.enable_columnstore = true)` failed with row-security errors.

## Options considered

1. Disable RLS on the source and use native continuous aggregates and columnstore. This has familiar
   Timescale operations but drops a required tenant boundary at the database.
2. Move raw and rollup data to different tenant policy regimes. This adds copies and retention
   coordination and still leaves a tenant-bearing Timescale table without RLS.
3. Keep RLS on Timescale rowstore tables, maintain incremental rollups in the owning transaction,
   and compress variable-length values with PostgreSQL TOAST.

## Decision

Use option 3. Stream-processor owns, migrates and writes the TimescaleDB store. Customer metrics
and check results use rowstore hypertables with RLS and time partitioning. Any hypertable uniqueness
constraint covers its partition keys and `organization_id`. Rollup tables have tenant policies too.
After deduplication, the writer updates the affected time buckets in the same tenant transaction as
the new samples. Use the retention periods in the data architecture for each tier. Batched writes
can combine samples by bucket before updating a rollup.

Keep numeric values and indexed dimensions queryable. Use TOAST compression for variable-length
telemetry payloads such as histograms and structured attributes. Track the size and write/read cost of
these columns under the milestone workload. This design trades columnar compression and the native
refresh policy for the same RLS enforcement on raw and rolled-up data.

Query-service owns the authorized, bounded query API. It reaches stream-processor through a
deadline-bound, mTLS-authenticated read RPC. Stream-processor runs the database read under the
bound tenant and checks the tenant, project, time range and page limits at its own edge.
SQL grants for that read path are SELECT-only.

## Verification

- Run the schema checks and direct cross-tenant integration tests for raw data and every rollup tier.
  Include an unset tenant, a foreign resource, and a connection reused by another tenant.
- Read with the runtime role after compressing representative large payload values. Assert tenant
  filtering and round-trip values and record actual bytes before and after compression.
- Redeliver committed records and add late samples in integration tests. Verify a unique raw result,
  correct incremental rollup values, offset progress, and retention for both raw and aggregate data.
- Verify the mTLS read boundary and capture query plans and memory/throughput under M03 load.
