-- Migration: 000001_init
-- Description: registry (projects, api_keys, sheets), the write-behind copy (cache_entries), and the
--              cluster epoch (D-012). Source: doc/persist/design.md, D-013, D-015.
-- Date: 2026-10-29
--
-- ============================================================================
-- WORKFLOW OVERVIEW
-- ============================================================================
--
-- 1. BOOT (REDLITE_PERSIST_ENABLED=true, leader role):
--    a. golang-migrate applies this folder (embedded in the binary)
--    b. ListSheets            → every sheet and its bcrypt password hash, into memory
--    c. ListLiveEntries       → every row whose expires_at is NULL or in the future, per sheet
--    d. GetEpoch              → the fencing epoch this leader resumes at
--    e. the WS listener starts only after b–d
--
-- 2. WRITE-BEHIND (internal/persist writer, one goroutine):
--    memory first → op enqueued → batch of REDLITE_PERSIST_BATCH_SIZE or REDLITE_PERSIST_FLUSH_INTERVAL
--    → one transaction of UpsertEntry / DeleteEntry / DeleteEntriesByPrefix / DeleteEntriesBySuffix.
--
-- 3. TENANCY: application-level. Every tenant table carries project_id and every query filters on it.
--    Sheets autocreated by a v1 `open` (REDLITE_SHEET_AUTOCREATE=true) belong to the built-in
--    'default' project seeded below. No RLS (doc/persist/design.md decision 6).
--
-- 4. cache_entries is UNLOGGED: memory is the source of truth for reads; losing this table on a
--    crash costs a cold start, not data. projects, api_keys, sheets and cluster_state are logged.
-- ============================================================================

-- Projects: one tenant of the cache service (backendAdvo is one).
CREATE TABLE projects (
  id          uuid        PRIMARY KEY,
  name        text        NOT NULL UNIQUE,
  created_at  timestamptz NOT NULL DEFAULT now(),
  quota_keys  bigint,                         -- NULL = unlimited
  quota_bytes bigint                          -- NULL = unlimited
);

-- The project that owns every autocreated sheet (D-013).
INSERT INTO projects (id, name) VALUES ('00000000-0000-0000-0000-000000000000', 'default');

-- API keys: issued on project creation, SHA-256 at rest, the first 8 characters kept for logs.
CREATE TABLE api_keys (
  id          uuid        PRIMARY KEY,
  project_id  uuid        NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  key_hash    bytea       NOT NULL UNIQUE,    -- sha256(key)
  key_prefix  text        NOT NULL,           -- first 8 chars, for logs and listing
  created_at  timestamptz NOT NULL DEFAULT now(),
  revoked_at  timestamptz
);

-- Sheets: a sheet outlives its connections (K13). The bcrypt hash is what open checks (K12, K14).
CREATE TABLE sheets (
  project_id    uuid        NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  name          text        NOT NULL,
  password_hash bytea       NOT NULL,         -- bcrypt
  autocreated   boolean     NOT NULL DEFAULT false,
  created_at    timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (name),
  UNIQUE (project_id, name)
);

-- The durable copy of every sheet's entries. value is the exact JSON the client sent.
CREATE UNLOGGED TABLE cache_entries (
  project_id  uuid        NOT NULL,
  sheet       text        NOT NULL,
  key         text        NOT NULL,
  value       jsonb       NOT NULL,
  expires_at  timestamptz,                    -- NULL = no expiry (REDLITE_CACHE_TTL=0)
  updated_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (project_id, sheet, key),
  FOREIGN KEY (project_id, sheet) REFERENCES sheets(project_id, name) ON DELETE CASCADE
);
CREATE INDEX cache_entries_expires_idx ON cache_entries (expires_at) WHERE expires_at IS NOT NULL;

-- Cluster state: one row, the fencing epoch (D-012). Cluster-wide, so no project_id.
CREATE TABLE cluster_state (
  id          integer     PRIMARY KEY CHECK (id = 1),
  epoch       bigint      NOT NULL,
  instance_id text        NOT NULL,
  updated_at  timestamptz NOT NULL DEFAULT now()
);
