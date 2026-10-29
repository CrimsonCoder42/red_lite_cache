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
INSERT INTO api_keys (id, project_id, key_hash, key_prefix)
VALUES ($1, $2, $3, $4)
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
