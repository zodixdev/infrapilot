-- name: InsertAuditEvent :exec
INSERT INTO audit_events (id, organization_id, actor_kind, actor_id, action, target_type, target_id, details, request_id, trace_id)
VALUES (@id, @organization_id, @actor_kind, @actor_id, @action, @target_type, @target_id, @details, @request_id, @trace_id);

-- name: ListAuditEvents :many
-- Newest first; @before is the last id of the previous page (the max UUID for the first page).
SELECT * FROM audit_events
WHERE organization_id = @organization_id AND id < @before
ORDER BY id DESC
LIMIT @lim;
