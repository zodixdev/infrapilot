-- name: InsertOrganization :one
INSERT INTO organizations (id, name) VALUES (@id, @name) RETURNING *;

-- name: GetOrganization :one
SELECT * FROM organizations WHERE id = @id AND organization_id = @id AND deleted_at IS NULL;

-- name: ListOrganizationsForUser :many
-- Runs under WithUserTx (ADR-0023): RLS limits rows to the caller's memberships.
SELECT o.id, o.name, o.created_at, o.updated_at, m.role
FROM memberships m
JOIN organizations o ON o.id = m.organization_id
WHERE m.user_id = @user_id AND o.deleted_at IS NULL AND m.organization_id > @after
ORDER BY m.organization_id
LIMIT @lim;

-- name: UpdateOrganization :one
UPDATE organizations
SET name = coalesce(sqlc.narg(name), name), updated_at = now()
WHERE id = @id AND organization_id = @id AND deleted_at IS NULL
RETURNING *;

-- name: LockLiveOrganization :one
-- Writes that must not outlive the organization's deletion (API keys) share-lock its row; the delete's row lock
-- waits for them, and their later revocation step then sees them. A concurrent rename waits for the (short)
-- key-creating transaction as well.
SELECT id FROM organizations WHERE id = @id AND organization_id = @id AND deleted_at IS NULL FOR SHARE;

-- name: SoftDeleteOrganization :execrows
UPDATE organizations SET deleted_at = now(), updated_at = now()
WHERE id = @id AND organization_id = @id AND deleted_at IS NULL;
