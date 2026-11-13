-- name: InsertProject :one
INSERT INTO projects (id, organization_id, name, slug, description)
VALUES (@id, @organization_id, @name, @slug, @description)
RETURNING *;

-- name: GetProject :one
SELECT * FROM projects WHERE organization_id = @organization_id AND id = @id AND deleted_at IS NULL;

-- name: ListProjects :many
SELECT * FROM projects
WHERE organization_id = @organization_id AND id > @after AND deleted_at IS NULL
ORDER BY id
LIMIT @lim;

-- name: UpdateProject :one
UPDATE projects
SET name        = coalesce(sqlc.narg(name), name),
    slug        = coalesce(sqlc.narg(slug), slug),
    description = coalesce(sqlc.narg(description), description),
    updated_at  = now()
WHERE organization_id = @organization_id AND id = @id AND deleted_at IS NULL
RETURNING *;

-- name: SoftDeleteProject :execrows
UPDATE projects SET deleted_at = now(), updated_at = now()
WHERE organization_id = @organization_id AND id = @id AND deleted_at IS NULL;
