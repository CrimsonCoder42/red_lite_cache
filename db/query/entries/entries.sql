-- ================================
-- CACHE ENTRIES: the write-behind copy (S5, doc/persist/design.md)
-- ================================
-- Every query filters on project_id (application-level tenancy). Prefix and suffix matches use
-- starts_with / right(), never LIKE, so a key containing % or _ cannot widen the match.

-- name: UpsertEntry :exec
INSERT INTO cache_entries (project_id, sheet, key, value, expires_at, updated_at)
VALUES ($1, $2, $3, $4, $5, now())
ON CONFLICT (project_id, sheet, key) DO UPDATE
SET value = EXCLUDED.value, expires_at = EXCLUDED.expires_at, updated_at = now();

-- name: DeleteEntry :exec
DELETE FROM cache_entries
WHERE project_id = $1 AND sheet = $2 AND key = $3;

-- name: DeleteEntriesByPrefix :execrows
DELETE FROM cache_entries
WHERE project_id = @project_id AND sheet = @sheet AND starts_with(key, @prefix::text);

-- name: DeleteEntriesBySuffix :execrows
DELETE FROM cache_entries
WHERE project_id = @project_id AND sheet = @sheet AND right(key, length(@suffix::text)) = @suffix::text;

-- name: ListLiveEntries :many
-- Hydration: rows already expired are skipped, not loaded-then-expired.
SELECT key, value, expires_at FROM cache_entries
WHERE project_id = $1 AND sheet = $2 AND (expires_at IS NULL OR expires_at > now());

-- name: DeleteExpiredEntries :execrows
-- The TTL cleaner: at most 10000 rows a pass so a backlog never holds a long lock. Cluster-wide by
-- design (it touches only rows that are already dead for every project).
DELETE FROM cache_entries
WHERE ctid IN (
  SELECT ctid FROM cache_entries
  WHERE expires_at IS NOT NULL AND expires_at < now()
  LIMIT 10000
);

-- name: DeleteEntries :exec
-- One janitor expiry pass for one sheet (OpExpire): every key in one statement.
DELETE FROM cache_entries
WHERE project_id = @project_id AND sheet = @sheet AND key = ANY(@keys::text[]);
