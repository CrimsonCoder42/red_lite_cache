-- PROPOSED migration pair — NOT in db/migration. The migration guard treats 000001 as shipped and the
-- bootstrap pre-cleared only 000001, so this needs Devin's approval (MIGRATION CHECKPOINT).
-- To apply: copy the UP block to db/migration/000002_audit.up.sql and the DOWN block to
-- db/migration/000002_audit.down.sql, add db/query/audit/audit.sql (below) and a sqlc.yaml block
-- (package "audit", same overrides), then `make sqlc`.
-- Source: D-019, doc/persist/postgres_role.md §2.1 and §3.1.

-- ===== 000002_audit.up.sql =====
-- Migration: 000002_audit
-- Description: audit of admin actions and cluster events. Never read on the hot path. Partitioned by
--              month on `at`, BRIN on `at`; the app creates this and next month's partition at boot.
CREATE TABLE audit_events (
  at          timestamptz NOT NULL DEFAULT now(),
  project_id  uuid,                           -- NULL for cluster events
  actor       text        NOT NULL,           -- 'admin', 'key:<prefix>', 'instance:<id>'
  action      text        NOT NULL,           -- project_created, sheet_created, project_revoked, promoted, stepped_down
  subject     text        NOT NULL,           -- project id, sheet name or instance id
  detail      jsonb       NOT NULL DEFAULT '{}'::jsonb -- identifiers only: never a key, hash or value
) PARTITION BY RANGE (at);
CREATE TABLE audit_events_default PARTITION OF audit_events DEFAULT;
CREATE INDEX audit_events_at_brin ON audit_events USING brin (at);

-- ===== 000002_audit.down.sql =====
DROP TABLE IF EXISTS audit_events;

-- ===== db/query/audit/audit.sql =====
-- name: InsertAuditEvent :exec
-- INSERT INTO audit_events (project_id, actor, action, subject, detail) VALUES ($1, $2, $3, $4, $5);
--
-- name: ListAuditEvents :many
-- SELECT * FROM audit_events WHERE at >= $1 ORDER BY at DESC LIMIT $2;
