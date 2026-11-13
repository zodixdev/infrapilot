-- Retention (ADR-0027). Runs as <svc>_batch (BYPASSRLS) through WithSystemTx, in bounded batches so each
-- statement holds locks briefly. Each query returns the number of rows deleted.

-- name: PurgeOrganizations :execrows
DELETE FROM organizations WHERE id IN (
    SELECT id FROM organizations WHERE deleted_at < now() - interval '30 days' ORDER BY deleted_at LIMIT @lim);

-- name: PurgeProjects :execrows
DELETE FROM projects WHERE id IN (
    SELECT id FROM projects WHERE deleted_at < now() - interval '30 days' ORDER BY deleted_at LIMIT @lim);

-- name: PurgeResources :execrows
DELETE FROM resources WHERE id IN (
    SELECT id FROM resources WHERE deleted_at < now() - interval '30 days' ORDER BY deleted_at LIMIT @lim);

-- name: PurgeAPIKeys :execrows
DELETE FROM api_keys WHERE id IN (
    SELECT id FROM api_keys
    WHERE (revoked_at IS NOT NULL OR expires_at IS NOT NULL)
      AND coalesce(revoked_at, expires_at) < now() - interval '30 days'
    ORDER BY coalesce(revoked_at, expires_at) LIMIT @lim);

-- name: PurgeAuditEvents :execrows
DELETE FROM audit_events WHERE id IN (
    SELECT id FROM audit_events WHERE created_at < now() - interval '400 days' ORDER BY created_at LIMIT @lim);

-- name: PurgeIdempotencyKeys :execrows
DELETE FROM idempotency_keys WHERE (organization_id, principal_id, key) IN (
    SELECT organization_id, principal_id, key FROM idempotency_keys
    WHERE created_at < now() - interval '24 hours' ORDER BY created_at LIMIT @lim);

-- name: PurgeUsers :execrows
DELETE FROM users WHERE id IN (
    SELECT u.id FROM users u
    WHERE u.last_seen_at < now() - interval '400 days'
      AND NOT EXISTS (SELECT 1 FROM memberships m WHERE m.user_id = u.id)
    ORDER BY u.last_seen_at LIMIT @lim);
