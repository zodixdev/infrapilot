-- name: GetRole :one
-- Authorization lookup (ADR-0022): the caller's role in a live organization.
SELECT m.role
FROM memberships m
JOIN organizations o ON o.id = m.organization_id
WHERE m.organization_id = @organization_id AND m.user_id = @user_id AND o.deleted_at IS NULL;

-- name: InsertMembership :exec
INSERT INTO memberships (organization_id, user_id, role, created_by)
VALUES (@organization_id, @user_id, @role, sqlc.narg(created_by));

-- name: GetMember :one
SELECT m.organization_id, m.user_id, m.role, m.created_at, m.updated_at, u.email, u.display_name
FROM memberships m JOIN users u ON u.id = m.user_id
WHERE m.organization_id = @organization_id AND m.user_id = @user_id;

-- name: LockMember :one
-- Locks the target membership row for a role change or removal.
SELECT role FROM memberships
WHERE organization_id = @organization_id AND user_id = @user_id
FOR UPDATE;

-- name: LockOwners :many
-- Locks every owner row so concurrent demotions/removals cannot both pass the last-owner check.
SELECT user_id FROM memberships
WHERE organization_id = @organization_id AND role = 'owner'
ORDER BY user_id
FOR UPDATE;

-- name: ListMembers :many
SELECT m.organization_id, m.user_id, m.role, m.created_at, m.updated_at, u.email, u.display_name
FROM memberships m JOIN users u ON u.id = m.user_id
WHERE m.organization_id = @organization_id AND m.user_id > @after
ORDER BY m.user_id
LIMIT @lim;

-- name: UpdateMemberRole :exec
UPDATE memberships SET role = @role, updated_at = now()
WHERE organization_id = @organization_id AND user_id = @user_id;

-- name: DeleteMember :execrows
DELETE FROM memberships WHERE organization_id = @organization_id AND user_id = @user_id;
