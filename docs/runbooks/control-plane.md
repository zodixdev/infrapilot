# Runbook — control-plane

Owns organizations, projects, memberships, resources, API keys and the audit log (PostgreSQL database
`controlplane`, ADR-0018 roles). Serves internal gRPC only, to allow-listed callers (ADR-0025); answers
`CheckPermission` for the edge (ADR-0022). Retention runs as the hourly CronJob `control-plane-purge` (ADR-0027).

## Where to look

- Dashboard **InfraPilot / control-plane** (`observability/grafana/dashboards/control-plane.json`): RPC rate,
  errors, latency by method, pool usage, peer denials, purge results, certificate expiry.
- Logs: JSON with `trace_id`; `principal metadata from a non-edge peer` and `grpc peer not allowed` are security
  relevant; `retention purge completed` lists rows deleted per table.
- `/readyz` depends on Postgres (DB-bound service; see ServiceNotReady in the platform runbook).

## Alerts

### ControlPlanePurgeStale
**Meaning:** the retention job has not succeeded for over 3 hours (or never reported). Soft-deleted data, expired keys,
old audit events and idempotency records accumulate.
**Check:** `kubectl -n <ns> get cronjob control-plane-purge` and its last jobs (`kubectl get jobs -l
app.kubernetes.io/component=job-purge`); job logs (`retention purge failed` names the table); the `controlplane_batch`
credential (`batch-url` in the `control-plane-db` Secret); job egress to Postgres and to the OTLP collector (the job
pushes its metrics — if only the push fails, data is purged but this alert still fires).
**Mitigate:** fix the cause and trigger a run: `kubectl create job --from=cronjob/control-plane-purge purge-manual`.
Runs are idempotent and resume where they stopped.

### ControlPlanePeerDenied
**Meaning:** a workload keeps calling an RPC the allow-list denies.
**Check:** control-plane logs `grpc peer not allowed` (method and peer identity; logged at most once per second per
method). An identity that should be allowed → `EDGE_IDENTITIES` / Helm values or a renamed ServiceAccount; an unknown
identity → treat as a security incident (who issued that certificate?).
**Mitigate:** fix the configuration; for unknown callers isolate the workload (NetworkPolicy) and revoke its certificate.

### ControlPlaneRPCLatencyP99High
**Meaning:** an RPC's p99 exceeds 250 ms.
**Check:** `PgPoolSaturated`/`PgPoolWaiting`; slow queries (`log_min_duration_statement`, `pg_stat_statements`);
lock waits on `memberships` (owner locks serialize membership changes per organization by design).
**Mitigate:** scale replicas within the connection budget (data.md §8); compare query plans with the EXPLAIN evidence of
the milestone report; add the missing index through a migration.

## Operations

`INGESTION_IDENTITIES` enables the credential-to-resource gRPC check for the listed telemetry-ingestion
SPIFFE identities. Leave it empty where telemetry-ingestion is absent. Keep this list distinct from
`EDGE_IDENTITIES`, which authorizes workloads to act for end users.

- **Restoring a deleted organization (within 30 days):** run as the migrator role (owner, bypasses RLS), in one
  transaction, recording who did it and why in the organization's audit log:
  ```sql
  BEGIN;
  UPDATE organizations SET deleted_at = NULL, updated_at = now() WHERE id = '<org>' AND deleted_at IS NOT NULL;
  INSERT INTO audit_events (id, organization_id, actor_kind, actor_id, action, target_type, target_id, details, request_id)
  VALUES (gen_random_uuid(), '<org>', 'user', '<operator user id>', 'org.restored', 'organization', '<org>',
          jsonb_build_object('ticket', '<incident or ticket id>'), '<ticket id>');
  COMMIT;
  ```
  API keys stay revoked — owners create new ones.
- **Removing a stuck membership:** never edit `memberships` directly; use the API so the change is audited.
- **Overlapping retention runs:** the CronJob forbids concurrency; a manually started run that overlaps a scheduled one
  waits on the other's row locks, fails on the 5-minute idle/lock limits without deleting anything twice, and the next
  run resumes. Check with `kubectl get jobs -l app.kubernetes.io/component=job-purge`.
- **Migrations:** applied by the Helm pre-upgrade hook (`control-plane migrate`) as `controlplane_migrator`; a lock
  timeout (another session holds a lock) is retried 5× with backoff before the job fails and the release aborts.
- **Credentials:** three roles (`_migrator`, `_app`, `_batch`) in Secret `control-plane-db` (`migrator-url`, `app-url`,
  `batch-url`). Rotate one role at a time; roll the matching workload.
