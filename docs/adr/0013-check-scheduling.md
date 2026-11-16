# ADR-0013: Lease-based distributed check scheduling with fencing

- Status: accepted · Decider: senior-backend · Date: 2026-11-12

## Context
50k checks at 30–60 s intervals across N check-runner replicas, no single leader, tolerant of replica crashes,
GC pauses and network stalls.

**Guarantee we can actually provide:** each scheduled run is executed **at least once** (possibly more than once
when a lease expires while a slow worker is still running). We do **not** claim exactly-once execution.
What we guarantee is **at most one effective result per scheduled run**: duplicates cannot change state twice.
Executing a probe twice is harmless (it is a read-only network check); recording/acting on it twice is not.

## Options
1. Single leader scheduler — bottleneck and failover gap.
2. Redis sorted sets — scheduling state in a non-durable store.
3. **Postgres `next_run_at` + lease + fencing token, claimed with `FOR UPDATE SKIP LOCKED` in batches.**
4. Kafka-partition ownership — couples scheduling to consumer rebalancing.

## Decision
Option 3.

- Each check row has `next_run_at`, `lease_owner`, `lease_until`, `lease_epoch BIGINT`.
- **Claim:** in one statement, select due rows (`next_run_at <= now() AND (lease_until IS NULL OR lease_until < now())`)
  `FOR UPDATE SKIP LOCKED LIMIT n`, set `lease_owner`, `lease_until = now() + timeout + margin`, `lease_epoch = lease_epoch + 1`,
  and return the new epoch. The epoch is the **fencing token**.
- Each run gets a deterministic **run key** `(check_id, scheduled_at)` where `scheduled_at` is the slot it was due for.
- **Complete:** `UPDATE ... SET next_run_at = <next slot + jitter>, lease_owner = NULL, lease_until = NULL
  WHERE id = $1 AND lease_epoch = $epoch`. If 0 rows are updated, the worker was fenced off: it discards its
  state change and increments `checkrunner_fenced_completions_total`.
- The result event always carries `run_key` and `lease_epoch`. Downstream consumers (stream-processor, incident-engine)
  dedupe on `run_key`; if two results for the same `run_key` arrive, the one with the higher `lease_epoch` wins and the
  other is dropped.
- Database time (`now()`) is the only clock used for leases, avoiding cross-node clock skew.
- Workers stop starting new probes when their lease is close to expiry; probe timeout < lease duration.
- Backpressure: claim size adapts to free worker-pool slots; nothing is claimed that cannot start immediately.
- Index: `(next_run_at) WHERE enabled`.

## Consequences
+ No leader, horizontal scaling, crash recovery by lease expiry, no double state change.
− Duplicate probes can happen under stalls (measured by `checkrunner_fenced_completions_total` and duplicate `run_key`
  metrics); acceptable because probes are idempotent reads. Postgres load grows with check count — measured in M10.

## Verification
- Stale-lease test: pause a worker past its lease (test hook), let another claim and complete, resume the first:
  its completion is fenced, its result is dropped downstream, exactly one effective result and at most one incident.
- M03 load gate uses 10k checks across 3 replicas on kind with schedule drift p95 < 5 s. Verify stale-worker
  fencing and recovery when a replica is killed.
- M10 validates 50k checks across 3 replicas. Kill a replica and require schedule drift p95 < 5 s and zero
  duplicate effective results.
