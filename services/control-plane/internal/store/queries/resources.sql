-- name: InsertResource :one
INSERT INTO resources (id, organization_id, project_id, kind, name, labels)
VALUES (@id, @organization_id, @project_id, @kind, @name, @labels)
RETURNING *;

-- name: GetResource :one
SELECT * FROM resources
WHERE organization_id = @organization_id AND project_id = @project_id AND id = @id AND deleted_at IS NULL;

-- name: ListResources :many
SELECT * FROM resources
WHERE organization_id = @organization_id AND project_id = @project_id AND id > @after AND deleted_at IS NULL
ORDER BY id
LIMIT @lim;

-- name: ListResourcesByKind :many
SELECT * FROM resources
WHERE organization_id = @organization_id AND project_id = @project_id AND kind = @kind AND id > @after AND deleted_at IS NULL
ORDER BY id
LIMIT @lim;

-- name: UpdateResource :one
UPDATE resources
SET name       = coalesce(sqlc.narg(name), name),
    labels     = coalesce(sqlc.narg(labels), labels),
    updated_at = now()
WHERE organization_id = @organization_id AND project_id = @project_id AND id = @id AND deleted_at IS NULL
RETURNING *;

-- name: SoftDeleteResource :execrows
UPDATE resources SET deleted_at = now(), updated_at = now()
WHERE organization_id = @organization_id AND project_id = @project_id AND id = @id AND deleted_at IS NULL;

-- name: SoftDeleteProjectResources :exec
UPDATE resources SET deleted_at = now(), updated_at = now()
WHERE organization_id = @organization_id AND project_id = @project_id AND deleted_at IS NULL;
