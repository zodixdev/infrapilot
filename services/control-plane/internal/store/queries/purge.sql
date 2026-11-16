-- Retention (ADR-0027). Runs as <svc>_batch (BYPASSRLS) through WithSystemTx, in bounded batches so each
-- statement holds locks briefly. Each query returns the number of rows deleted.

-- name: ListExpiredOrganizations :many
SELECT id FROM organizations WHERE deleted_at < now() - interval '30 days' ORDER BY deleted_at LIMIT @lim;

-- name: LockExpiredOrganization :one
SELECT lock_expired_organization(@org::uuid)::boolean AS expired;

-- An expired organization is emptied table by table in bounded batches, then deleted, so one large tenant
-- never becomes one unbounded cascading statement.

-- name: PurgeOrganizationResources :execrows
DELETE FROM resources WHERE id IN (SELECT r.id FROM resources r WHERE r.organization_id = @org LIMIT @lim);

-- name: PurgeOrganizationAPIKeys :execrows
DELETE FROM api_keys WHERE id IN (SELECT k.id FROM api_keys k WHERE k.organization_id = @org LIMIT @lim);

-- name: PurgeOrganizationProjects :execrows
DELETE FROM projects WHERE id IN (SELECT p.id FROM projects p WHERE p.organization_id = @org LIMIT @lim);

-- name: PurgeOrganizationMemberships :execrows
DELETE FROM memberships WHERE (organization_id, user_id) IN (
    SELECT m.organization_id, m.user_id FROM memberships m WHERE m.organization_id = @org LIMIT @lim);

-- name: PurgeOrganizationIdempotencyKeys :execrows
DELETE FROM idempotency_keys WHERE (organization_id, principal_id, key) IN (
    SELECT i.organization_id, i.principal_id, i.key FROM idempotency_keys i WHERE i.organization_id = @org LIMIT @lim);

-- name: DeleteOrganization :execrows
DELETE FROM organizations WHERE id = @id AND deleted_at < now() - interval '30 days';

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
-- Audit events outlive their organization (no foreign key) and age out after 400 days only.
DELETE FROM audit_events WHERE id IN (
    SELECT id FROM audit_events WHERE created_at < now() - interval '400 days' ORDER BY created_at LIMIT @lim);

-- name: PurgeIdempotencyKeys :execrows
DELETE FROM idempotency_keys WHERE (organization_id, principal_id, key) IN (
    SELECT organization_id, principal_id, key FROM idempotency_keys
    WHERE created_at < now() - interval '24 hours' ORDER BY created_at LIMIT @lim);

-- name: PurgeOrgCreateKeys :execrows
DELETE FROM org_create_keys WHERE (principal_id, key) IN (
    SELECT principal_id, key FROM org_create_keys
    WHERE created_at < now() - interval '24 hours' ORDER BY created_at LIMIT @lim);

-- name: PurgeUsers :execrows
-- SKIP LOCKED: a user being added to an organization right now holds a key-share lock and is skipped.
DELETE FROM users WHERE id IN (
    SELECT u.id FROM users u
    WHERE u.last_seen_at < now() - interval '400 days'
      AND NOT EXISTS (SELECT 1 FROM memberships m WHERE m.user_id = u.id)
    ORDER BY u.last_seen_at LIMIT @lim
    FOR UPDATE SKIP LOCKED);
