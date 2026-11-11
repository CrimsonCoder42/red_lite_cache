-- PROPOSED, not applied: doc/specs/s8_project_sheets.md § 2. For Devin to place as
-- db/migration/000003_project_sheets.up.sql (+ the down below) and to run make sqlc after the
-- db/query edits listed in the spec. Never run by an agent.
--
-- Sheet names become unique per project (S8 decision, 2026-11-11). Existing rows keep their
-- project_id, so no data moves: UNIQUE (project_id, name) already holds today (000001:59), and
-- cache_entries already references (project_id, sheet) (000001:71).

-- ===== up =====
BEGIN;

-- cache_entries' FK (000001:71) depends on the UNIQUE (project_id, name) index, so that index is
-- promoted to the primary key in place instead of being dropped (a plain DROP would fail with
-- "other objects depend on it").
ALTER TABLE sheets DROP CONSTRAINT sheets_pkey;              -- PRIMARY KEY (name)
ALTER TABLE sheets ADD CONSTRAINT sheets_pkey PRIMARY KEY USING INDEX sheets_project_id_name_key;

-- A project sheet has no password (S8 § 1). A hash stays only on a v1 sheet, for password-mode
-- opens, until v1 admission is retired.
ALTER TABLE sheets ALTER COLUMN password_hash DROP NOT NULL;

-- Name rule (S8 § 2): 1-128 bytes of [A-Za-z0-9._-].
ALTER TABLE sheets ADD CONSTRAINT sheets_name_rule
  CHECK (name ~ '^[A-Za-z0-9._-]{1,128}$') NOT VALID;          -- NOT VALID: legacy names untouched

-- Per-project creation counter for the sheet limit check is NOT stored: the limit is counted
-- from the registry in memory (S8 § 3).

COMMIT;

-- ===== down =====
-- BEGIN;
-- ALTER TABLE sheets DROP CONSTRAINT sheets_name_rule;
-- -- fails if two projects now share a name: that is the point of no return.
-- -- The FK depends on sheets_pkey's index now: drop it, swap the keys, re-add it.
-- ALTER TABLE cache_entries DROP CONSTRAINT cache_entries_project_id_sheet_fkey;
-- ALTER TABLE sheets DROP CONSTRAINT sheets_pkey;
-- ALTER TABLE sheets ADD PRIMARY KEY (name);
-- ALTER TABLE sheets ADD CONSTRAINT sheets_project_id_name_key UNIQUE (project_id, name);
-- ALTER TABLE cache_entries ADD CONSTRAINT cache_entries_project_id_sheet_fkey
--   FOREIGN KEY (project_id, sheet) REFERENCES sheets(project_id, name) ON DELETE CASCADE;
-- UPDATE sheets SET password_hash = '' WHERE password_hash IS NULL;
-- ALTER TABLE sheets ALTER COLUMN password_hash SET NOT NULL;
-- COMMIT;
