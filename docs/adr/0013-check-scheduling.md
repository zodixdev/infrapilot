# ADR-0013: Lease-based distributed check scheduling

- Status: accepted · Decider: senior-backend · Date: 2026-11-12

## Context
50k checks at 30–60 s intervals across N check-runner replicas; every check must run once per interval,
even when replicas die, without a single leader.

## Options
1. Single leader scheduler — simple, but a bottleneck and failover gap.
2. Redis sorted sets — fast, but scheduling state then lives in a non-durable store.
3. **Postgres `next_run_at` + lease columns, claimed with `FOR UPDATE SKIP LOCKED` in batches.**
4. Kafka-partition ownership — couples scheduling to rebalancing.

## Decision
Option 3. Each replica loops: claim up to N due checks (`lease_owner`, `lease_until = now()+interval`),
push into a bounded worker pool, publish results to Kafka, advance `next_run_at` with jitter.
Expired leases are reclaimable. Worker pool size and claim batch adapt to pool saturation (backpressure).
Index: `(next_run_at) WHERE enabled`.

## Verification
Load test 50k checks / 3 replicas; kill a replica; measure schedule drift p95 (< 5 s) and duplicate runs (0 counted
per interval in the incident path thanks to idempotency).
