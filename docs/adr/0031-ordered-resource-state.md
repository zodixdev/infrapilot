# ADR-0031: Ordered publication of resource lifecycle state

- Status: accepted (refines ADR-0005 and ADR-0018) · Decider: claude-senior · Date: 2026-11-16

## Context

The scheduler needs a local view of which control-plane resources can run checks. It must account for
parent deletion, the 30-day recovery window, and resources already present in an M02 database. A late
retry of an older event can cause Kafka compaction to retain the wrong state even if a live consumer
previously saw the correct state.

## Alternatives

1. Publish snapshots from the RPC handlers after commit. A crash can lose a change and SQL restores and
   old binaries can bypass publication during deployment.
2. Publish a marker for each parent and have consumers join it to child state. This makes a parent
   change cheaper but requires a new Kafka key space and recovery rules for the parent and child join.
3. Capture effective resource state in the source database and dispatch in key order. Consumers can
   rebuild from one compacted topic. Organization lifecycle changes write one row per affected resource.

## Decision

Use option 3. `resource.state.v1` uses 12 fixed partitions and key `org_id/resource_id`. Its payload
contains project ownership, effective lifecycle status and a monotonically increasing revision per key.
Organization and resource IDs are in the envelope. Control-plane retains resource names, labels and
other metadata. Changes to those fields leave scheduler state unchanged.

### Transactional capture and upgrade

- Database triggers capture resource creation, soft deletion, hard deletion and changes to effective
  availability caused by an organization. The capture and source change commit or roll back together.
  The existing project delete soft-deletes its resources in the same transaction and uses the resource
  trigger. An organization delete or SQL restore captures all live resources of its live projects via
  a set-based insert. A restore leaves individually deleted resources and projects inactive.
- Resource writers take the organization share lock before the project lock on the M03 request path.
  Capture checks the organization row under a share lock for compatibility with an M02 writer.
  The organization trigger reads its children without acquiring child locks. A child writer that
  encounters a concurrent organization transition checks the committed parent state before enqueue.
  A hard delete always captures a purge marker. The capture sequence is assigned inside these source
  locks, so it orders state for each resource regardless of wall clock changes.
- Purge rechecks the 30-day deadline and holds a share lock on the organization during each destructive
  batch. A fixed-search-path function lets the batch role take this lock without a grant to update
  organizations. The restore procedure locks the same row before checking the recovery deadline.
- Install capture before backfill. Record the highest existing resource ID under the migration lock so
  the migration Job can finish its bounded keyset batches during sustained writes. Use a durable
  enrollment marker that survives outbox pruning and check current state under the same source locks
  before enqueuing. Keep provisioning and backfill progress visible to operators. Verify rollback,
  M02 writes during rollout, interrupted backfill, and parent/child races on real PostgreSQL.
- The capture table is service-internal outbox infrastructure with only identifiers, lifecycle,
  trace context and delivery metadata. Its capture insert runs inside the transaction that changes
  tenant state. This refines ADR-0018's `WithSystemTx` rule for infrastructure writes. Preparation and
  delivery use `WithSystemTx` in the owning service. The batch role may prune acknowledged rows after
  seven days. Pending rows survive until acknowledgment, with count and oldest-age alerts.

### Kafka delivery and replay

- Before network delivery, prepare each capture as a UUIDv7 Protobuf envelope in Go and commit the
  prepared bytes to PostgreSQL. Retries reuse those bytes. Each dispatcher acquires a PostgreSQL lock
  row for one partition using `FOR UPDATE SKIP LOCKED`. It selects at most one oldest pending record
  per key in a bounded batch. The Kafka transactional ID is stable per deployment, topic and partition.
  Fetch the Kafka producer epoch under the lock and recheck the same live PostgreSQL transaction
  before sending. Discard the producer on uncertain ownership or commit. Mark rows sent only after
  Kafka commits the batch. An ambiguous acknowledgment can duplicate a revision. It cannot put an
  older revision after a newer one because a batch contains only one pending head for that key.
- Readers use `read_committed`. An inactive record remains available for restore during the soft-delete
  window. A hard purge sends a Kafka tombstone, with the typed envelope in the `infrapilot-envelope`
  header to preserve the event identity and revision. Kafka retains tombstones for seven days before
  compaction can collect them. Resource IDs are never reused.
- Persist each partition's progress with its topic ID and a recent catch-up checkpoint against a sampled
  read-committed end offset. Missing or invalid progress, a topic ID change, or a checkpoint older than
  six days requires a fresh projection. Mark that partition unavailable to new scheduling, replay from
  its earliest offset into an empty generation, and switch generations and offsets atomically after
  reaching the sampled end. Abort a rebuild that reaches the six-day limit. Kafka offsets can remain
  in range after compaction removes tombstones, so offset validity alone cannot establish freshness.

## Consequences

The resource view is eventually consistent. Kafka compaction can keep the last committed state per key
without accumulating permanently deleted resource keys. Tenant writes retain availability during a Kafka
outage, subject to PostgreSQL capacity. Measure organization fan-out in the M03 load environment and
exercise ambiguous broker acknowledgments, stale producer fencing and stale projection rebuild against
the pinned broker. Preserve the partition count for the lifetime of the topic.

Kafka references: [topic configuration](https://kafka.apache.org/28/configuration/topic-level-configs/),
[producer transactions](https://kafka.apache.org/26/configuration/producer-configs/).
