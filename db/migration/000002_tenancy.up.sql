-- Migration: 000002_tenancy
-- Description: project suspension (D-027), key labels and last-use (D-028), and the audit log
--              (D-019, D-026: admin mutations and session open/close/kick). 000001 stays immutable.
-- Date: 2026-10-29
--
-- ============================================================================
-- WORKFLOW OVERVIEW
-- ============================================================================
--
-- 1. SUSPEND (POST /admin/projects/{id}/suspend): suspended_at = now(); every session of the project
--    is closed on every node (4001) and open is refused; sheets and entries stay. Resume sets NULL.
-- 2. KEYS: a project holds many keys; label names one for the operator; last_used_at is touched at
--    most once a minute per key by admission (never per frame).
-- 3. AUDIT: one row per admin mutation and per session open/close/kick. Never read on the hot path.
--    Partitioned by month on `at`; the app creates this and next month's partition at boot and on
--    the cleaner tick (internal/persist); anything outside them lands in the default partition.
--    detail holds identifiers only — never a key, hash, password or value.
-- ============================================================================

ALTER TABLE projects ADD COLUMN suspended_at timestamptz;

ALTER TABLE api_keys ADD COLUMN last_used_at timestamptz;
ALTER TABLE api_keys ADD COLUMN label text NOT NULL DEFAULT '';

CREATE TABLE audit_events (
  at          timestamptz NOT NULL DEFAULT now(),
  node        text        NOT NULL,           -- instance id that wrote the row
  project_id  uuid,                           -- NULL for cluster events
  actor       text        NOT NULL,           -- 'admin', 'key:<prefix>', 'instance:<id>'
  action      text        NOT NULL,           -- project_created, project_suspended, key_revoked, session_open, session_kick, …
  subject     text        NOT NULL,           -- the project id, key id, sheet name or session id acted on
  detail      jsonb       NOT NULL DEFAULT '{}'::jsonb
) PARTITION BY RANGE (at);
CREATE TABLE audit_events_default PARTITION OF audit_events DEFAULT;
CREATE INDEX audit_events_at_brin ON audit_events USING brin (at);
