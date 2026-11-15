-- PROPOSED, NOT APPLIED by agents: Devin copies it to db/migration/000003_admin.up.sql (the down half is
-- 000003_admin.down.proposed.sql). D1–D8 decided 2026-11-12. 000001 and 000002 stay immutable.
--
-- Migration: 000003_admin
-- Description: admin portal users, the one-shot bootstrap latch, cookie sessions, lockout counters.
--              Projects, api_keys and audit_events (000001, 000002) carry everything else unchanged.
-- Date: 2026-11-12
--
-- ============================================================================
-- WORKFLOW OVERVIEW
-- ============================================================================
--
-- 1. BOOTSTRAP (POST /admin/bootstrap, admin key): admin_bootstrap() inserts the latch row id=1 and
--    the first super_admin in one transaction. A second call hits the latch PK and returns NULL → 410.
--    The latch is never deleted, so disabling every user does not reopen bootstrap.
-- 2. SIGN-IN (POST /admin/auth/login): Go reads admin_user_for_login(email), compares the bcrypt hash
--    (D1, cost 12), then admin_login_record(id, ok) updates the counters; 5 failures lock 15 minutes.
-- 3. SESSION: the cookie holds 32 random bytes; only sha256(cookie) is stored (same as api_keys,
--    000001:45). admin_session_check() validates idle + absolute expiry and slides last_seen_at in
--    one UPDATE … RETURNING. Disable a user = DELETE their sessions in the same transaction.
-- 4. AUDIT: no new table. audit_events (000002) gets actor 'user:<uuid>' and admin_* actions.
-- 5. password_hash is a bcrypt string ($2a$, 60 chars) made in Go (D1); Postgres never sees a password.
-- ============================================================================

CREATE TABLE admin_users (
  id                   uuid        PRIMARY KEY DEFAULT gen_random_uuid(),  -- core since PG13
  email                text        NOT NULL CHECK (length(email) BETWEEN 3 AND 254),
  name                 text        NOT NULL DEFAULT '',
  role                 text        NOT NULL CHECK (role IN ('super_admin', 'admin', 'viewer')),
  password_hash        text        NOT NULL,           -- bcrypt, cost 12; never logged, never returned
  must_change_password boolean     NOT NULL DEFAULT false,  -- set on create/reset by another admin
  failed_logins        integer     NOT NULL DEFAULT 0,
  locked_until         timestamptz,
  disabled_at          timestamptz,                    -- disable, never delete: audit keeps the id
  created_by           uuid        REFERENCES admin_users(id),
  created_at           timestamptz NOT NULL DEFAULT now(),
  password_changed_at  timestamptz NOT NULL DEFAULT now(),
  last_login_at        timestamptz
);
CREATE UNIQUE INDEX admin_users_email_key ON admin_users (lower(email));

-- The one-shot latch. One row ever; its PK is what makes bootstrap race-safe.
CREATE TABLE admin_bootstrap (
  id       integer     PRIMARY KEY CHECK (id = 1),
  user_id  uuid        NOT NULL,
  done_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE admin_sessions (
  token_hash   bytea       PRIMARY KEY,                -- sha256(cookie value)
  user_id      uuid        NOT NULL REFERENCES admin_users(id) ON DELETE CASCADE,
  created_at   timestamptz NOT NULL DEFAULT now(),
  last_seen_at timestamptz NOT NULL DEFAULT now(),
  expires_at   timestamptz NOT NULL,                   -- absolute cap (12 h)
  ip           inet,
  user_agent   text        NOT NULL DEFAULT ''
);
CREATE INDEX admin_sessions_user_idx    ON admin_sessions (user_id);
CREATE INDEX admin_sessions_expires_idx ON admin_sessions (expires_at);

-- Returns the new super_admin's id, or NULL when bootstrap already ran.
CREATE FUNCTION admin_bootstrap(p_email text, p_name text, p_hash text) RETURNS uuid
LANGUAGE plpgsql AS $$
DECLARE uid uuid := gen_random_uuid();
BEGIN
  INSERT INTO admin_bootstrap (id, user_id) VALUES (1, uid);  -- the latch: blocks a concurrent caller
  INSERT INTO admin_users (id, email, name, role, password_hash)
  VALUES (uid, p_email, p_name, 'super_admin', p_hash);
  RETURN uid;
EXCEPTION WHEN unique_violation THEN
  RETURN NULL;
END $$;

-- The row the sign-in check needs; no row for an unknown email (Go then compares a dummy hash).
CREATE FUNCTION admin_user_for_login(p_email text)
RETURNS TABLE (id uuid, password_hash text, role text, locked_until timestamptz, disabled_at timestamptz)
LANGUAGE sql STABLE AS $$
  SELECT id, password_hash, role, locked_until, disabled_at FROM admin_users WHERE lower(email) = lower(p_email)
$$;

-- Records one attempt; returns locked_until (NULL = not locked). Success resets the counter.
CREATE FUNCTION admin_login_record(p_user uuid, p_ok boolean, p_max integer, p_lock interval)
RETURNS timestamptz LANGUAGE sql AS $$
  UPDATE admin_users SET
    failed_logins = CASE WHEN p_ok OR failed_logins + 1 >= p_max THEN 0 ELSE failed_logins + 1 END,
    locked_until  = CASE WHEN p_ok THEN NULL
                         WHEN failed_logins + 1 >= p_max THEN now() + p_lock
                         ELSE locked_until END,
    last_login_at = CASE WHEN p_ok THEN now() ELSE last_login_at END
  WHERE id = p_user
  RETURNING locked_until
$$;

CREATE FUNCTION admin_session_open(p_hash bytea, p_user uuid, p_ip inet, p_ua text, p_ttl interval)
RETURNS timestamptz LANGUAGE sql AS $$
  INSERT INTO admin_sessions (token_hash, user_id, expires_at, ip, user_agent)
  VALUES (p_hash, p_user, now() + p_ttl, p_ip, left(p_ua, 256))
  RETURNING expires_at
$$;

-- Every portal request: one indexed UPDATE. No row = 401 (expired, idle, unknown or user disabled).
CREATE FUNCTION admin_session_check(p_hash bytea, p_idle interval)
RETURNS TABLE (user_id uuid, email text, role text, must_change_password boolean)
LANGUAGE sql AS $$
  UPDATE admin_sessions s SET last_seen_at = now()
  FROM admin_users u
  WHERE s.token_hash = p_hash AND u.id = s.user_id
    AND s.expires_at > now() AND s.last_seen_at > now() - p_idle AND u.disabled_at IS NULL
  RETURNING u.id, u.email, u.role, u.must_change_password
$$;

-- Cleaner tick (internal/persist): drop dead sessions.
CREATE FUNCTION admin_sessions_purge(p_idle interval) RETURNS bigint
LANGUAGE sql AS $$
  WITH d AS (DELETE FROM admin_sessions WHERE expires_at <= now() OR last_seen_at <= now() - p_idle RETURNING 1)
  SELECT count(*) FROM d
$$;
