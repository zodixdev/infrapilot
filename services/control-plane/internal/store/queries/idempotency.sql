-- name: GetIdempotencyKey :one
SELECT * FROM idempotency_keys
WHERE organization_id = @organization_id AND principal_id = @principal_id AND key = @key;

-- name: InsertIdempotencyKey :exec
INSERT INTO idempotency_keys (organization_id, principal_id, key, operation, request_hash, resource_id)
VALUES (@organization_id, @principal_id, @key, @operation, @request_hash, @resource_id);
