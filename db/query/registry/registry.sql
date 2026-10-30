-- ================================
-- REGISTRY: projects, api_keys, sheets (S6, D-013)
-- ================================

-- name: CreateProject :one
-- Creates a project; the API layer issues its first API key in the same transaction.
INSERT INTO projects (id, name)
VALUES ($1, $2)
RETURNING *;

-- name: GetProject :one
SELECT * FROM projects
WHERE id = $1;

-- name: CreateAPIKey :one
-- key_hash is sha256 of the key; the key itself is returned to the caller once and never stored.
INSERT INTO api_keys (id, project_id, key_hash, key_prefix, label)
VALUES (@id, @project_id, @key_hash, @key_prefix, @label)
RETURNING id, project_id, key_prefix, created_at;

-- name: GetActiveAPIKeyByHash :one
-- Resolves a presented key (hashed by the caller) to its project; a revoked key is not found.
SELECT id, project_id, key_prefix FROM api_keys
WHERE key_hash = $1 AND revoked_at IS NULL;

-- name: UpsertSheet :exec
-- Created by the admin API or by an autocreating open (project 'default'). A repeat keeps the
-- original project and replaces the hash only when the name already belongs to the same project.
INSERT INTO sheets (project_id, name, password_hash, autocreated)
VALUES ($1, $2, $3, $4)
ON CONFLICT (name) DO UPDATE
SET password_hash = EXCLUDED.password_hash
WHERE sheets.project_id = EXCLUDED.project_id;

-- name: GetSheet :one
SELECT * FROM sheets
WHERE project_id = $1 AND name = $2;

-- name: ListSheets :many
-- Hydration: every sheet, with its hash, before the listener starts.
SELECT * FROM sheets
ORDER BY name;

-- name: ListProjectSheets :many
SELECT * FROM sheets
WHERE project_id = $1
ORDER BY name;

-- name: RevokeProjectKeys :execrows
-- Revokes every live key of a project (POST /admin/projects/{id}/revoke).
UPDATE api_keys SET revoked_at = now()
WHERE project_id = $1 AND revoked_at IS NULL;

-- name: DeleteProjectSheets :many
-- Removes a revoked project's sheets (entries cascade) and returns their names so every instance
-- can drop them from memory (D-021 control bus).
DELETE FROM sheets
WHERE project_id = $1
RETURNING name;

-- ================================
-- TENANCY (000002: D-027 suspend, D-028 keys, D-029 listing)
-- ================================

-- name: SetProjectSuspended :one
-- Suspends (suspended = true) or resumes a project; returns the stored suspended_at (NULL on resume).
UPDATE projects
SET suspended_at = CASE WHEN @suspended::boolean THEN COALESCE(suspended_at, now()) ELSE NULL END
WHERE id = @id
RETURNING suspended_at;

-- name: ListSuspendedProjects :many
-- Boot and reconcile: the projects whose sessions must be refused.
SELECT id FROM projects
WHERE suspended_at IS NOT NULL;

-- name: ListProjectsPage :many
-- GET /admin/projects: keyset page on (created_at, id), with live key and sheet counts.
SELECT p.id, p.name, p.created_at, p.suspended_at,
       (SELECT count(*) FROM api_keys k WHERE k.project_id = p.id AND k.revoked_at IS NULL)::int AS keys,
       (SELECT count(*) FROM sheets s WHERE s.project_id = p.id)::int AS sheets
FROM projects p
WHERE (p.created_at, p.id) > (@after_created_at::timestamptz, @after_id::uuid)
ORDER BY p.created_at, p.id
LIMIT @page_limit;

-- name: ListProjectKeys :many
-- GET /admin/projects/{id}: every key of the project (revoked included), never the hash.
SELECT id, key_prefix, label, created_at, revoked_at, last_used_at
FROM api_keys
WHERE project_id = $1
ORDER BY created_at, id;

-- name: GetAPIKey :one
SELECT id, project_id, key_prefix, label, created_at, revoked_at, last_used_at
FROM api_keys
WHERE id = $1;

-- name: RevokeAPIKey :one
-- DELETE /admin/keys/{id} and the old half of rotate: returns the project, or no row when the key
-- does not exist or was already revoked.
UPDATE api_keys SET revoked_at = now()
WHERE id = $1 AND revoked_at IS NULL
RETURNING project_id;

-- name: TouchAPIKeyUsed :exec
-- Admission: at most one write a minute per key (D-028), never per frame.
UPDATE api_keys SET last_used_at = now()
WHERE id = $1 AND (last_used_at IS NULL OR last_used_at < now() - interval '1 minute');
