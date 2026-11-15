-- ================================
-- ADMIN PORTAL (000003, S11): admin users, the bootstrap latch, cookie sessions, the audit read
-- ================================
-- Passwords are hashed in Go (bcrypt, D1); these queries only ever carry the hash. Session tokens
-- arrive as sha256(cookie). No query returns password_hash except AdminUserForLogin and
-- AdminUserPasswordHash, which the sign-in and password-change checks need.
-- Durations travel as whole seconds and become intervals here, so Go never builds a pgtype.Interval.
-- A parameter followed by * uses sqlc.arg(x), not @x: Postgres parses @ as the prefix operator, which binds
-- looser than *, so "@x::int * interval" would make sqlc rewrite the whole product as the parameter.

-- name: AdminBootstrapDone :one
-- POST /admin/bootstrap answers 410 early when the latch row exists (admin_bootstrap() is the race-safe check).
SELECT EXISTS (SELECT 1 FROM admin_bootstrap) AS done;

-- name: AdminBootstrap :one
-- The first super_admin and the latch row in one transaction; uuid.Nil when bootstrap already ran.
SELECT COALESCE(admin_bootstrap(@email::text, @name::text, @password_hash::text),
                '00000000-0000-0000-0000-000000000000'::uuid)::uuid AS user_id;

-- name: AdminUserForLogin :one
-- No row for an unknown email. NULL timestamps come back as flags / the epoch: the function's columns
-- are typed by their casts, which sqlc makes non-null, and a NULL cannot scan into time.Time.
SELECT l.id::uuid AS id, l.password_hash::text AS password_hash, l.role::text AS role,
       COALESCE(l.locked_until, 'epoch'::timestamptz)::timestamptz AS locked_until,
       (l.disabled_at IS NOT NULL)::boolean AS disabled
FROM admin_user_for_login(@email::text) AS l;

-- name: AdminLoginRecord :one
-- One sign-in attempt; returns locked_until, the epoch when not locked (never NULL, see AdminUserForLogin).
SELECT COALESCE(admin_login_record(@user_id::uuid, @ok::boolean, @max_failures::int,
                                   sqlc.arg(lock_seconds)::int * interval '1 second'),
                'epoch'::timestamptz)::timestamptz AS locked_until;

-- name: AdminSessionOpen :one
SELECT admin_session_open(@token_hash::bytea, @user_id::uuid, NULLIF(@ip::text, '')::inet,
                          @user_agent::text, sqlc.arg(ttl_seconds)::int * interval '1 second')::timestamptz AS expires_at;

-- name: AdminSessionCheck :one
-- No row = no valid session (unknown, idle, past its absolute expiry, or its user is disabled).
SELECT c.user_id::uuid AS user_id, c.email::text AS email, c.role::text AS role,
       c.must_change_password::boolean AS must_change_password
FROM admin_session_check(@token_hash::bytea, sqlc.arg(idle_seconds)::int * interval '1 second') AS c;

-- name: AdminSessionsPurge :one
-- Cleaner tick (internal/persist): expired and idle sessions.
SELECT admin_sessions_purge(sqlc.arg(idle_seconds)::int * interval '1 second')::bigint AS purged;

-- name: AdminSessionDelete :exec
-- POST /admin/auth/logout.
DELETE FROM admin_sessions WHERE token_hash = @token_hash::bytea;

-- name: AdminSessionsDeleteForUser :execrows
-- Disable, reset and password change: every session of the user except keep (empty = all).
DELETE FROM admin_sessions WHERE user_id = @user_id::uuid AND token_hash <> @keep::bytea;

-- name: AdminUserGet :one
SELECT id, email, name, role, must_change_password, failed_logins, locked_until, disabled_at,
       created_by, created_at, password_changed_at, last_login_at
FROM admin_users
WHERE id = @id::uuid;

-- name: AdminUserPasswordHash :one
-- POST /admin/auth/password: the caller's current hash (an active user only).
SELECT password_hash FROM admin_users WHERE id = @id::uuid AND disabled_at IS NULL;

-- name: AdminUsersPage :many
-- GET /admin/users: keyset page on (created_at, id), never the hash.
SELECT id, email, name, role, must_change_password, failed_logins, locked_until, disabled_at,
       created_by, created_at, password_changed_at, last_login_at
FROM admin_users
WHERE (created_at, id) > (@after_created_at::timestamptz, @after_id::uuid)
ORDER BY created_at, id
LIMIT @page_limit;

-- name: AdminUserCreate :one
-- POST /admin/users: a unique violation on lower(email) is 409. created_by is NULL when the bearer
-- admin key (no user) creates the user.
INSERT INTO admin_users (email, name, role, password_hash, must_change_password, created_by)
VALUES (@email::text, @name::text, @role::text, @password_hash::text, true, sqlc.narg(created_by)::uuid)
RETURNING id, email, name, role, must_change_password, failed_logins, locked_until, disabled_at,
          created_by, created_at, password_changed_at, last_login_at;

-- name: AdminUserSetPassword :execrows
-- Password change (must_change = false) and reset (must_change = true); clears the lockout.
UPDATE admin_users
SET password_hash = @password_hash::text, must_change_password = @must_change::boolean,
    password_changed_at = now(), failed_logins = 0, locked_until = NULL
WHERE id = @id::uuid;

-- name: AdminActiveSuperAdminsLock :many
-- Inside the PATCH transaction: locks every active super_admin row so two concurrent demotions
-- cannot both see "another super_admin remains" (the second re-checks after the first commits).
SELECT id FROM admin_users
WHERE role = 'super_admin' AND disabled_at IS NULL
ORDER BY id
FOR UPDATE;

-- name: AdminUserUpdate :one
-- PATCH /admin/users/{id}: role and/or disabled; a NULL argument leaves the field as it is.
UPDATE admin_users
SET role = COALESCE(sqlc.narg(role)::text, role),
    disabled_at = CASE
      WHEN sqlc.narg(disabled)::boolean IS NULL THEN disabled_at
      WHEN sqlc.narg(disabled)::boolean THEN COALESCE(disabled_at, now())
      ELSE NULL END
WHERE id = @id::uuid
RETURNING id, email, name, role, must_change_password, failed_logins, locked_until, disabled_at,
          created_by, created_at, password_changed_at, last_login_at;

-- name: AuditPage :many
-- GET /admin/audit: newest first, optional project and action filters, keyset on
-- (at, node, action, subject) descending (audit_events has no id; the four together are the tiebreak).
SELECT at, node, project_id, actor, action, subject, detail
FROM audit_events
WHERE at >= @since::timestamptz
  AND (sqlc.narg(project_id)::uuid IS NULL OR project_id = sqlc.narg(project_id)::uuid)
  AND (sqlc.narg(action)::text IS NULL OR action = sqlc.narg(action)::text)
  AND (NOT @has_cursor::boolean
       OR (at, node, action, subject) < (@cur_at::timestamptz, @cur_node::text, @cur_action::text, @cur_subject::text))
ORDER BY at DESC, node DESC, action DESC, subject DESC
LIMIT @page_limit;
