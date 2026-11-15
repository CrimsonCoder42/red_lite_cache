# S11 Admin portal, backend — Spec

**Status:** verified 2026-11-12 (backend slices S1–S5, `doc/admin_portal_2026-11-12/REPORT.md`; harness with Postgres 444 | 441 | 1 | 2, the one failure pre-existing; the portal app S6–S8 is a later wave)
**Reference:** new. Plan: `doc/admin_portal_2026-11-12/PLAN.md` §1–§8. Shape of users/sessions after
`backendAdvo/db/migration/000001_init_schema.up.sql:89-137` (read-only); its bootstrap is deliberately not copied
(per tenant, no transaction: `backendAdvo/api/handlers/admin/bootstrap.go:70-98`).

## Goal

Operators sign in to the admin port with an email and password (an opaque session cookie), with three fixed roles,
a one-shot bootstrap of the first super_admin, rate limits and lockout. The bearer admin key keeps working as
super_admin for scripts, `docker/bootstrap/bootstrap.sh` and the peer node.

## Acceptance criteria

1. `POST /admin/bootstrap` with the bearer admin key creates the first super_admin exactly once; every later or
   concurrent call answers 410, including 10 concurrent callers (one wins). Without the admin key: 401.
2. `POST /admin/auth/login` answers the same status and the byte-identical body for an unknown email, a wrong
   password, a disabled user and a locked user; an unknown email still costs one bcrypt compare.
3. 5 consecutive failures lock the account for 15 min; a success resets the counter.
4. A session cookie is `HttpOnly`, `SameSite=Strict`, `Path=/`, `Secure` unless `REDLITE_ENVIRONMENT=development`,
   `Max-Age` = 12 h. A session ends after 30 min idle or 12 h absolute, on logout, on disable, on a password reset.
5. Every route in the table below refuses a caller below its role with 403 and an unauthenticated caller with 401.
6. While `must_change_password` is set, only `POST /admin/auth/password`, `GET /admin/auth/me` and
   `POST /admin/auth/logout` answer; everything else is 403 "password change required".
7. An unsafe-method request carrying the session cookie and a cross-origin `Sec-Fetch-Site`/`Origin` is 403
   (`http.CrossOriginProtection`). Bearer callers are not browsers and are not checked.
8. Every admin mutation's audit row names the real actor: `user:<uuid>` for a cookie caller, `admin` for the key.
9. Rate limits answer 429 with `Retry-After`; the limiter map never holds more than 10 000 entries.
10. The last active super_admin cannot be demoted or disabled (409), also under two concurrent requests.
11. A temporary password (create, reset) is returned once and sets `must_change_password`.
12. Persistence off: the routes that need Postgres (bootstrap, auth/*, users, audit) answer 503
    "persistence required"; every pre-existing route and `GET /admin/stats` work as before with the bearer key.
13. No password, cookie, token, admin key or hash reaches a log line or an audit row.

## Surface: routes (admin port, `internal/api`)

Role order: viewer < admin < super_admin. The bearer admin key = super_admin, actor `admin`. "key" = the project's
own API key (unchanged S6 behaviour). Rate classes: **ip** = per client IP 20 rps burst 40 (every /admin route; the bearer admin key skips it, D-055);
**user** = per signed-in user 10 rps burst 20 (cookie callers); **login** = per IP 5 per minute burst 5.

| Method | Path | Min role | Rate | Needs DB |
|---|---|---|---|---|
| POST | /admin/bootstrap | admin key only | ip, login | yes |
| POST | /admin/auth/login | none | ip, login | yes |
| POST | /admin/auth/logout | viewer (cookie) | ip, user | yes |
| GET | /admin/auth/me | viewer | ip, user | yes |
| POST | /admin/auth/password | viewer (cookie) | ip, user, login | yes |
| GET | /admin/users | super_admin | ip, user | yes |
| POST | /admin/users | super_admin | ip, user | yes |
| PATCH | /admin/users/{id} | super_admin | ip, user | yes |
| POST | /admin/users/{id}/reset-password | super_admin | ip, user | yes |
| GET | /admin/stats | viewer | ip, user | no |
| GET | /admin/audit | viewer | ip, user | yes |
| GET | /admin/projects, /admin/projects/{id} | viewer | ip, user | no |
| GET | /admin/sessions, /admin/sheets/{name}/sessions | viewer | ip, user | no |
| POST | /admin/projects, …/{id}/keys, …/{id}/suspend, …/{id}/resume, …/{id}/revoke | admin | ip, user | no |
| POST, DELETE | /admin/keys/{id}/rotate, /admin/keys/{id} | admin | ip, user | no |
| DELETE | /admin/sessions/{id} | admin | ip, user | no |
| POST | /admin/projects/{id}/sheets | admin or key | ip, user | no |
| GET | /admin/sheets/{id}/export | super_admin or key (D8) | ip, user | no |

## Value-source table

| Value | Source | Decided |
|---|---|---|
| bcrypt cost 12 | Devin D1 (2026-11-12) | ✅ |
| password length 15–72 bytes | NIST SP 800-63B-4 §3.1.1.2 (single-factor ≥ 15); bcrypt's 72-byte limit (`x/crypto/bcrypt` ErrPasswordTooLong; pgcrypto Table F.18) | ✅ |
| cookie flags, idle 30 min, absolute 12 h | Devin D2 | ✅ |
| lockout 5 failures → 15 min | PLAN.md §8, accepted with D1–D8 | ✅ |
| rate 20/40 ip, 10/20 user, 5/min login, map cap 10 000, sweep 10 min | the S3 request text | ✅ |
| temporary password: 18 random bytes, base64url (24 chars) | ≥ the 15-char minimum; crypto/rand | ✅ |
| session token: 32 random bytes, stored sha256 | the API-key scheme (`internal/api/api.go:287-299`) | ✅ |
| client IP | `RemoteAddr`; last `X-Forwarded-For` hop only when RemoteAddr is loopback and `REDLITE_ADMIN_TRUSTED_PROXY=true` (D6) | ✅ |
| `started_at` | process start, recorded by `api.New` | ✅ |

## Failure behavior

| Failure | What the caller sees | What the system does |
|---|---|---|
| Persist unavailable (off) | 503 "persistence required" on DB routes | bearer routes unchanged |
| Postgres error | 500 "internal error" | one Error log line, no secret in it |
| Peer down (stats) | 200, this node's entry plus `{node, error}` for the peer | Warn log |
| Limiter map full | 429 for a new IP/user until the sweep frees entries | existing entries keep working |
| Replica | auth and users work on both nodes (shared Postgres); project writes stay leader-only (503) | — |

## Reuse

`internal/api/api.go` `fail`, `decode`, `writeJSON`, `hashKey`; `tenancy.go` `pageParams`, `cursor`, `pathID`,
`fmtTime`, `internalError`; `sessions.go` `peer` (merge + `?scope=local`); `internal/persist/store.go` `RunCleaner`
after-funcs; `internal/app/app.go` `Stats()`; `golang.org/x/crypto/bcrypt` (already a dependency).

## Decisions

D1–D8 (Devin, 2026-11-12, PLAN.md §10) → D-051…D-055. D-040 (stats are counts only), D-041 (bounded maps),
D-047 (bootstrap.sh keeps the admin key), D-049 (admin port).

## Wire compatibility

The gRPC wire and `reader/` are untouched. Additive REST on the admin port only; the JSON types live in
`internal/api` (`protocol/` is not changed). Existing admin routes keep their request and response bodies; the
only visible change for them is 403 for a too-low role and 429 when a limit trips.

## Out of scope

`portal/` (S6–S8), serving `REDLITE_PORTAL_DIR` (config only), email/invites/forgot-password, per-project write
rates, the gRPC path.

## Tests

Black-box over `httptest` on the admin mux against real Postgres (`REDLITE_TEST_DATABASE_URL`, a fresh schema per
test migrated with `db.Migrate`, as `internal/persist/db_test.go:21-66` does). Any of them would run unchanged
against another implementation of the same REST surface, except TestLimiterMapBounded and TestLoginRecordLocksAtFive
(package internals).

| Criterion | Tests | File |
|---|---|---|
| 1 | TestBootstrapOnce, TestBootstrapConcurrent, TestBootstrapNeedsAdminKey, TestBootstrapGoneAfterFirst | internal/api/admin_auth_test.go |
| 2, 3 | TestUnknownEmailSameReply, TestLoginRecordLocksAtFive | internal/api/admin_auth_test.go |
| 4 | TestLoginCookieFlags, TestSessionCheckIdleAbsoluteDisabled, TestLogoutRevokes | internal/api/admin_auth_test.go |
| 5 | TestRoleMatrix | internal/api/admin_auth_test.go |
| 6 | TestMustChangeBlocks | internal/api/admin_auth_test.go |
| 7 | TestCrossOriginPostRefused | internal/api/admin_auth_test.go |
| 8 | TestAuditActorIsUser | internal/api/admin_auth_test.go |
| 9 | TestLoginRateLimitPerIP, TestPerUserLimit, TestAdminKeySkipsIPLimit, TestLimiterMapBounded, TestLimiterRetryAfter, TestTrustedProxyOnlyFromLoopback | internal/adminauth/adminauth_test.go, internal/api/admin_auth_test.go |
| 10, 11 | TestSuperAdminCreatesUser, TestAdminCannotManageUsers, TestDisableKillsSessions, TestLastSuperAdminProtected, TestResetForcesChange | internal/api/admin_users_test.go |
| 12 | TestPersistenceOffDegradesOnlyPortal + the existing `internal/api` suite (MemStore, no DB) | internal/api/admin_auth_test.go, api_test.go |
| input edges | TestPortalInputEdges (NUL → 4xx, bootstrap email taken → 409), TestPasswordRules | internal/api/admin_auth_test.go, internal/adminauth/adminauth_test.go |
| reads | TestStatsMergedTwoNodes, TestAuditFilters, TestSessionsPurged; TestStatsCountsOnly (running App, two key shapes) | internal/api/admin_reads_test.go; testing/integration/admin_portal_test.go |
