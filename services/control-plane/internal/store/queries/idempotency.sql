-- name: GetIdempotencyKey :one
-- Keys are honored for 24 hours (ADR-0024), independent of when the hourly purge runs.
SELECT * FROM idempotency_keys
WHERE organization_id = @organization_id AND principal_id = @principal_id AND key = @key
  AND created_at > now() - interval '24 hours';

-- name: InsertIdempotencyKey :exec
INSERT INTO idempotency_keys (organization_id, principal_id, key, operation, request_hash, resource_id)
VALUES (@organization_id, @principal_id, @key, @operation, @request_hash, @resource_id);
