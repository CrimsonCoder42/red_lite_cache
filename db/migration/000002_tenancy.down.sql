-- Migration: 000002_tenancy (down) — drops everything 000002_tenancy.up.sql added.
DROP TABLE IF EXISTS audit_events;
ALTER TABLE api_keys DROP COLUMN IF EXISTS label;
ALTER TABLE api_keys DROP COLUMN IF EXISTS last_used_at;
ALTER TABLE projects DROP COLUMN IF EXISTS suspended_at;
