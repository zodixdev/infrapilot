-- name: GetIdempotencyKey :one
-- Keys are honored for 24 hours (ADR-0024), independent of when the hourly purge runs.
SELECT * FROM idempotency_keys
WHERE organization_id = @organization_id AND principal_id = @principal_id AND key = @key
  AND created_at > now() - interval '24 hours';

-- name: LockIdempotencyKey :exec
-- Serializes requests with the same (organization, principal, key) for the rest of the transaction, so a
-- concurrent duplicate waits and then replays instead of colliding on the resource's own unique keys.
SELECT pg_advisory_xact_lock(hashtextextended(@scope::text, 0));

-- name: InsertIdempotencyKey :execrows
-- Replaces an expired record (older than the 24 h window but not yet purged); 0 rows means a live record exists.
INSERT INTO idempotency_keys (organization_id, principal_id, key, operation, request_hash, resource_id)
VALUES (@organization_id, @principal_id, @key, @operation, @request_hash, @resource_id)
ON CONFLICT (organization_id, principal_id, key) DO UPDATE
    SET operation = EXCLUDED.operation, request_hash = EXCLUDED.request_hash, resource_id = EXCLUDED.resource_id, created_at = now()
    WHERE idempotency_keys.created_at <= now() - interval '24 hours';

-- name: GetOrgCreateKey :one
SELECT * FROM org_create_keys
WHERE principal_id = @principal_id AND key = @key AND created_at > now() - interval '24 hours';

-- name: InsertOrgCreateKey :execrows
INSERT INTO org_create_keys (principal_id, key, organization_id, request_hash)
VALUES (@principal_id, @key, @organization_id, @request_hash)
ON CONFLICT (principal_id, key) DO UPDATE
    SET organization_id = EXCLUDED.organization_id, request_hash = EXCLUDED.request_hash, created_at = now()
    WHERE org_create_keys.created_at <= now() - interval '24 hours';
