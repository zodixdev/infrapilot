-- name: InsertAPIKey :one
INSERT INTO api_keys (id, organization_id, project_id, name, prefix, scopes, created_by, expires_at)
VALUES (@id, @organization_id, @project_id, @name, @prefix, @scopes, @created_by, sqlc.narg(expires_at))
RETURNING *;

-- name: InsertAPIKeyIndex :exec
INSERT INTO api_key_index (key_hash, key_id, organization_id, project_id, scopes, expires_at)
VALUES (@key_hash, @key_id, @organization_id, @project_id, @scopes, sqlc.narg(expires_at));

-- name: GetAPIKey :one
SELECT * FROM api_keys WHERE organization_id = @organization_id AND id = @id;

-- name: ListAPIKeys :many
SELECT * FROM api_keys
WHERE organization_id = @organization_id AND id > @after
ORDER BY id
LIMIT @lim;

-- name: ListProjectAPIKeys :many
SELECT * FROM api_keys
WHERE organization_id = @organization_id AND project_id = @project_id AND id > @after
ORDER BY id
LIMIT @lim;

-- name: RevokeAPIKey :one
-- The api_keys_mirror_revocation trigger updates api_key_index in the same transaction.
UPDATE api_keys SET revoked_at = coalesce(revoked_at, now())
WHERE organization_id = @organization_id AND id = @id
RETURNING *;

-- name: RevokeProjectAPIKeys :execrows
UPDATE api_keys SET revoked_at = now()
WHERE organization_id = @organization_id AND project_id = @project_id AND revoked_at IS NULL;

-- name: RevokeOrganizationAPIKeys :execrows
UPDATE api_keys SET revoked_at = now()
WHERE organization_id = @organization_id AND revoked_at IS NULL;

-- name: LookupAPIKey :one
-- Pre-tenant lookup (system transaction): only active keys authenticate.
SELECT key_id, organization_id, project_id, scopes, expires_at
FROM api_key_index
WHERE key_hash = @key_hash AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > now());

-- name: GetActiveAPIKeyScopes :one
-- Authoritative check inside the tenant (ADR-0022): the key is active and its project is live.
SELECT k.project_id, k.scopes
FROM api_keys k
JOIN projects p ON p.organization_id = k.organization_id AND p.id = k.project_id
JOIN organizations o ON o.id = k.organization_id
WHERE k.organization_id = @organization_id AND k.id = @id
  AND k.revoked_at IS NULL AND (k.expires_at IS NULL OR k.expires_at > now())
  AND p.deleted_at IS NULL AND o.deleted_at IS NULL;
