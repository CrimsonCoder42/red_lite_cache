-- ================================
-- AUDIT (000002, D-019, D-026): admin mutations and session open/close/kick
-- ================================
-- detail carries identifiers only — never a key, hash, password or cached value.

-- name: InsertAuditEvent :exec
INSERT INTO audit_events (at, node, project_id, actor, action, subject, detail)
VALUES (@at, @node, @project_id, @actor, @action, @subject, @detail);

-- name: ListAuditEvents :many
-- Newest first since a time; tests and operators.
SELECT at, node, project_id, actor, action, subject, detail
FROM audit_events
WHERE at >= @since
ORDER BY at DESC
LIMIT @page_limit;
