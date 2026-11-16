# ADR-0007: TimescaleDB for metrics, ClickHouse for logs/spans/events

- Status: accepted · Decider: senior-backend · Date: 2026-11-12

## Context
Two query shapes:
- Metrics/check results: narrow rows, time-range per series, rollups, joins with small dimension tables,
  threshold evaluation.
- Logs/spans/events: wide, high-volume, full-text-ish filtering and aggregations across millions of rows.

## Options
1. Everything in PostgreSQL — will not hold logs volume or retention.
2. Everything in ClickHouse — fine for analytics, but weaker for per-series rollup policies and SQL joins we want for metrics.
3. Prometheus/Mimir for customer metrics — multi-tenant ops complexity; not a SQL store for incident logic.
4. **TimescaleDB (hypertables, continuous aggregates, retention/compression policies) + ClickHouse (MergeTree, TTL).**

## Decision
Option 4. Writes only from stream-processor (batched), reads only through query-service. For
TimescaleDB, ADR-0030 supersedes the native continuous aggregate and columnstore choice and defines
the owner-served read path.

## Verification (M06/M10)
Benchmark both stores with the capacity design point; record ingest rate, storage per day, p95 query
latency. If TimescaleDB cannot meet targets, supersede with an ADR moving metrics to ClickHouse.
