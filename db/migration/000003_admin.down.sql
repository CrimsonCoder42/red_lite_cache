-- Migration: 000003_admin (down). PROPOSED: Devin copies it to db/migration/000003_admin.down.sql.
-- Drops the admin portal's users, bootstrap latch and sessions. audit_events rows written by portal
-- users stay (actor 'user:<uuid>' is plain text).

DROP FUNCTION IF EXISTS admin_sessions_purge(interval);
DROP FUNCTION IF EXISTS admin_session_check(bytea, interval);
DROP FUNCTION IF EXISTS admin_session_open(bytea, uuid, inet, text, interval);
DROP FUNCTION IF EXISTS admin_login_record(uuid, boolean, integer, interval);
DROP FUNCTION IF EXISTS admin_user_for_login(text);
DROP FUNCTION IF EXISTS admin_bootstrap(text, text, text);

DROP TABLE IF EXISTS admin_sessions;
DROP TABLE IF EXISTS admin_bootstrap;
DROP TABLE IF EXISTS admin_users;
