# Admin portal: plan (2026-11-12, research only, nothing built)

Goal (Devin): one portal modeled on advomedix_admin; a one-shot bootstrap to super admin, then locked; admin users
with roles; projects with a show-once API key; live usage; revoke; salted hashes; middleware + rate limit on
/admin; Postgres does the heavy lifting, Go stays thin, no viper, no new frameworks. Adv = `advo_production/advomedix-v2`.

## 0. What exists (verified)
- 15 `/admin` routes on the admin port, `internal/api/api.go:102-117`, all behind one bearer key compared with
  `subtle.ConstantTimeCompare` (`api.go:128-147`); `adminOnly` = that key (`tenancy.go:34-40`). The key is also
  what the peer node is called with (`sessions.go:46`) and what `docker/bootstrap/bootstrap.sh:18` uses (D-047).
- Admin port `REDLITE_ADMIN_PORT` default `":8097"` = all interfaces (`internal/config/config.go:170`), plain HTTP
  (D-049), `ReadHeaderTimeout` only (`internal/app/app.go:229`). Compose binds it to 127.0.0.1 (`docker-compose.yml:52,94`).
- Tables: `projects`, `api_keys` (sha256 at rest, `000001_init.up.sql:42-49`, `api.go:296-299`), `sheets`,
  `cluster_state` (000001); `suspended_at`, `label`, `last_used_at`, `audit_events` partitioned (`000002_tenancy.up.sql:20-35`).
  The prompt's "000001/000002" names are `000001_init` / `000002_tenancy`.
- Audit: `InsertAuditEvent`, `ListAuditEvents(since, limit)` (`db/query/audit/audit.sql:6-16`); actor is hard-coded
  `"admin"` for admin-key mutations (`tenancy.go:53-55`).
- Live data: `session.Info` (project, sheet, key, remote, connected, last_seen, frames in/out, node,
  `internal/session/session.go:79-91`); `App.Stats()` incl. persist queue/parked/dropped (`app.go:194-211`), today only
  on `/health` in development to a cluster-token holder (`app.go:183`).
- Earlier decision: `doc/persist/postgres_role.md:31` rejected pgcrypto auth (plaintext to the DB); `:32` RLS not v1;
  `doc/persist/design.md:35` app-level tenancy, no RLS. Sheet passwords already use Go bcrypt (`internal/sheet/registry.go:79`).

## 1. Data model (`000003_admin.proposed.sql`, not applied)
New: `admin_users` (email unique on lower(), role CHECK in 3 values, bcrypt `password_hash`, `must_change_password`,
`failed_logins`, `locked_until`, `disabled_at`, `created_by`), `admin_bootstrap` (one-row latch), `admin_sessions`
(sha256 of cookie, idle + absolute expiry, ip, ua). SQL functions: `admin_bootstrap`, `admin_user_for_login`,
`admin_login_record`, `admin_session_open`, `admin_session_check`, `admin_sessions_purge`. sqlc package `db/query/admin`.
- Reused from adv: users table shape (`000001:89-102` minus phone/image/tenant), sessions table with ip/ua/expiry
  (`:126-137`) but hashed token instead of the raw refresh token, `super_admin` role name.
- Dropped from adv: `roles`, `user_roles`, `permissions`, `role_permissions` (3 fixed roles = a CHECK), tenants,
  `password_reset_tokens` (no email), JWT `token/`. No `login_attempts` table: counters on the user row + audit rows.
- Carried by existing tables: projects/keys unchanged; every portal action → `audit_events` with actor `user:<uuid>`
  (admin key stays `admin`), actions `admin_bootstrap|admin_login|admin_login_failed|admin_logout|admin_user_*`.

## 2. Where hashing and the sign-in check happen
- **Recommend: bcrypt in Go (cost 12, `x/crypto` already in go.mod), lockout/session/audit in Postgres.** Go calls
  `admin_user_for_login` → `bcrypt.CompareHashAndPassword` (dummy hash when no row, so timing does not reveal emails)
  → `admin_login_record`. Two round trips, the password never leaves the Go process.
- Alternative: pgcrypto `crypt(pw, gen_salt('bf',12))` + `admin_sign_in()` (signature in the .sql). Lost because the
  manual says "all the data and passwords move between pgcrypto and client applications in clear text… Connect
  locally or use SSL… If you cannot, then better do crypto inside client application" (pgcrypto F.26.8.3), compose runs
  `sslmode=disable` (`docker-compose.yml:22`), a `log_statement=all`/slow-query log with bind values would carry the
  password (our inference; the page does not mention logs), bf defaults to cost 6 (Table F.19), and a login flood
  becomes DB CPU. It is reversible: both write the same `$2a$` string, so switching later needs no rehash.
- API keys: already sha256 in Go and looked up by hash (`api.go:139`, `registry.sql:21-24`); a 256-bit random key
  needs no salt or bcrypt. Unchanged. Session tokens use the same scheme.

## 3. Bootstrap
`POST /admin/bootstrap {email, name, password}`. Gate: the existing bearer admin key (only Devin has it) + persistence on.
`admin_bootstrap()` inserts latch row id=1 then the user in one transaction; a second or concurrent call gets the PK
conflict → NULL → **410 Gone** forever (the latch is never deleted). Not copied: adv's bootstrap checks super_admins per
tenant with no transaction (`backendAdvo/api/handlers/admin/bootstrap.go:70-98`), so it never disables and can race.
No new env flag: the admin key already is the secret; a flag adds config for no extra safety.
Lock-down sequence: (1) set `REDLITE_ADMIN_PORT=127.0.0.1:8097` (or the tailnet IP) and firewall 8097/8098;
(2) call bootstrap from the host with the body in a file (as `bootstrap.sh` does, not on the command line);
(3) sign in to the portal; (4) confirm a second bootstrap answers 410; (5) rotate `REDLITE_ADMIN_KEY` on both nodes
(`doc/release.md:94`) because the old one was typed into a shell; (6) optionally add a second super_admin;
(7) check `/admin/audit` shows `admin_bootstrap` + `admin_login`. Break-glass: the admin key acts as super_admin and
can reset any user's password.

## 4. Portal auth
- **Recommend: opaque session cookie**, 32 random bytes, `HttpOnly`, `Secure` (off only in development),
  `SameSite=Strict`, `Path=/`, idle 30 min, absolute 12 h; logout and disable delete the row (instant revoke).
- adv does JWT HS256 15 min + 24 h refresh, tokens in localStorage (`advomedix_admin/src/api/tokenManager.ts:12-53`),
  refresh by hand (`:126-183`), a blocked session's access token lives 15 min (`backendAdvo/api/middleware/auth.go:83-89`),
  refresh token stored raw. Not copied: XSS-readable storage, no instant revoke, a refresh flow to maintain.
- CSRF: stdlib `http.CrossOriginProtection` (Go 1.25+, go.mod is 1.27; Sec-Fetch-Site / Origin vs Host) on unsafe
  methods, plus SameSite=Strict and JSON-only bodies (`decode` already `DisallowUnknownFields`, `api.go:270-278`).
- Bearer admin key: **keep** for scripts, `bootstrap.sh`, peer calls, break-glass; it maps to super_admin. One
  `authenticate` resolves either credential into `caller{role, actor}`; handlers keep their bodies.

## 5. Users and roles (3, no permission tables)
| role | may |
|---|---|
| viewer | read projects, keys (prefix/label/last use), sessions, stats, audit |
| admin | viewer + create project/sheet, issue/rotate/revoke key, suspend/resume, revoke project, kill session |
| super_admin | admin + users (create, role, disable, reset password) + sheet export (cached values) |

Adding a user: super_admin enters email, name, role → server generates a temporary password, returns it **once**
(same dialog as API keys), sets `must_change_password`; that user can only call `/auth/password` until changed. No
email invites (no SMTP). Guard: the last active super_admin cannot be demoted or disabled. Disable deletes sessions.

## 6. Projects and keys in the UI: existing handlers, unchanged bodies
Create project → `POST /admin/projects` returns `api_key` once (`api.go:184-186`) → modal with copy button and a
ready `.env` line (`REDLITE_API_KEY=…`), closed = gone. Issue `POST /admin/projects/{id}/keys`, rotate
`POST /admin/keys/{id}/rotate`, revoke `DELETE /admin/keys/{id}`, suspend/resume `POST …/suspend|resume`, revoke
project `POST /admin/projects/{id}/revoke` (typed-name confirm). Only change: `s.audit` takes the caller's actor.

## 7. Monitoring
Today, no new code: project list with live key and sheet counts + suspended (`GET /admin/projects`), project detail
with keys' `last_used_at` and sheets' entry counts and collaborators (`GET /admin/projects/{id}`), sessions merged
across both nodes with frames in/out and last seen (`GET /admin/sessions`, `/admin/sheets/{name}/sessions`).
"Which projects are using the cache" = sessions grouped by project + key `last_used_at`.
New read endpoints:
- `GET /admin/stats` → `App.Stats()` (`app.go:194`) + role, epoch, instance, `started_at` (uptime), merged with the
  peer like sessions (`?scope=local`). Source: process memory per node. Counts only (D-040).
- `GET /admin/audit?since&project&action&limit&cursor` → `audit_events`, new sqlc query (filters + keyset).
- `GET /admin/auth/me`, `GET /admin/users` → `admin_users`.
- Deferred: per-project write rate. It needs counters on the sheet hot path; frames in/out per live session already
  give a proxy. Not worth touching `internal/sheet` for v1.

## 8. Protection on the admin port
Order: network (bind + firewall/Tailscale) → TLS terminated in front (`tailscale serve` or Caddy; the port stays
HTTP per D-049) → server limits (add `ReadTimeout` 15 s, `WriteTimeout` 30 s, `IdleTimeout` 60 s, `MaxHeaderBytes`
16 KiB to `app.go:229`) → per-IP rate limit → authenticate (cookie or bearer) → cross-origin check (cookie only) →
role check per route → per-user rate limit → handler (64 KiB body cap exists, `api.go:27`) → audit row in handler.
- Rate limiting: token bucket per IP (all /admin 20 rps burst 40; login 5/min) and per user (10 rps burst 20),
  `golang.org/x/time/rate` (D5), map capped at 10k entries with a 10-min idle sweep (D-041 style; code-heim's
  `go_82_rate_limiting_revised` has no eviction; adv's trusts the first `X-Forwarded-For` blindly, `rate_limit.go:236-267`).
  Counters in-process: free, but each node counts alone (2 nodes = 2× limit) and restarts reset them; acceptable for
  admin traffic. Lockout counters live in Postgres: shared by both nodes, survive restarts, auditable.
- Lockout: 5 failures → 15 min; the reply is always "sign-in failed" (no enumeration, no lock oracle).
- Client IP: `RemoteAddr`; behind a local proxy every request is 127.0.0.1, so one opt-in
  `REDLITE_ADMIN_TRUSTED_PROXY=true` takes the last `X-Forwarded-For` hop only when RemoteAddr is loopback (D6).
- Constant time: hash both sides to sha256 before `ConstantTimeCompare` on the admin key (it returns 0 immediately
  on a length mismatch, pkg.go.dev crypto/subtle) at `api.go:133`; dummy bcrypt for unknown emails.
- Cannot stop here: volumetric floods, scans, TLS stripping. That is the bind address (`127.0.0.1` or tailnet IP, never
  `:8097` in production), host firewall, Tailscale ACLs. Unauthenticated traffic costs one map lookup and a 401/429.

## 9. The portal app: `portal/` (top level)
Stack, trimmed from adv `package.json`: Vite 6, React 18, TS, Tailwind 4, react-router-dom 7, react-icons. Dropped:
Redux (auth = a React context fed by `/auth/me`), axios, toastify (small own toast), motion, d3, date-fns, stripe,
recaptcha, Playwright unless D10. Dev: Vite proxy `/admin` → `127.0.0.1:8097` (same origin, cookie works). Prod: Go
serves the built `portal/dist` from `REDLITE_PORTAL_DIR` at `/portal/` on the admin port (D4); empty = not served.
- Copy as-is (`advomedix_admin/src/features/…`): `ui/{Badge,Button,DataTable,EmptyState,Skeleton,StatTile,GlassCard,
  Input,SearchInput,FilterTabs,RowMenu}`, `layout/{AppShell,SidebarNavLink,SettingsMenu,ThemeToggle,Topbar}`,
  `theme/{themeStore,useAdminTheme}`, the pre-paint script `index.html:24-36`, tokens `src/index.css:118-223` (light,
  `html.dark`) + `@custom-variant dark` `:232` + `.admin-shell`/`.admin-card`.
- Trim: `ui/Modal` + `shared/modals` (key dialog), `ui/TablePager` (offset → cursor "next"), `ui/cells`,
  `layout/GlassSidebar` (drop authSlice), `adminNav` pattern (role-gated items), `admin_list/AdminListPage`
  (+ `domains.tsx` as the config pattern), `auth/components/ProtectedRoute` (roles), `auth/pages/LoginPage` (drop Google,
  images), `user_admin/UserDetailPage`.
- Leave: signup/verify/forgot/reset/newsletter pages, `tokenManager.ts`, `useAuth.ts` (542 lines), `authSlice`,
  `authService` (axios), billing 429 dispatch and `X-Patient-ID` (`apiClient.ts:37-41,92-98`), pointer flare, palette
  `index.css:22-99`, `roles.ts` groups beyond 3 roles.
- Pages: `/login` sign in · `/password` forced/voluntary change · `/` overview (stats per node, live sessions per
  project, persist queue/drops, uptime) · `/projects` list + create · `/projects/:id` keys, sheets, suspend/revoke ·
  `/sessions` live sessions, kill · `/users` (super_admin) · `/users/:id` role, disable, reset · `/audit` filterable log.
- API client contract: `fetch(path, {credentials:'same-origin'})`, JSON in/out, errors `{error}` (`protocol.APIError`),
  pages `{items, next_cursor}` (`protocol/admin.go:64-67`); 401 → `/login`, 403 → toast, 410/429 → message; no tokens
  in JS at all.

## 10. Build order (stop after each slice; PHASE 1 build, PHASE 2 verify = `make test` + named tests green)
Tests are black-box over HTTP (`httptest` on the admin mux) against real Postgres (`REDLITE_TEST_DATABASE_URL`).
| # | slice | tests (PHASE 2) | est |
|---|---|---|---|
| S1 | 000003 up/down, sqlc `admin` | TestAdminMigrationUpDown, TestBootstrapOnce, TestBootstrapConcurrent (10 callers → 1 id), TestLoginRecordLocksAtFive, TestSessionCheckIdleAbsoluteDisabled | 0.5 d |
| S2 | bootstrap, login/logout/me/password, cookie, authenticate + role per route, CSRF, audit actor | TestBootstrapNeedsAdminKey, TestBootstrapGoneAfterFirst, TestLoginCookieFlags, TestUnknownEmailSameReply, TestMustChangeBlocks, TestLogoutRevokes, TestCrossOriginPostRefused, TestRoleMatrix (every route × role), existing `internal/api` suite unchanged, TestAuditActorIsUser | 1.5 d |
| S3 | rate limits, lockout reply, server timeouts, sha256 admin-key compare | TestLoginRateLimitPerIP (429 + Retry-After), TestPerUserLimit, TestLimiterMapBounded, TestTrustedProxyOnlyFromLoopback | 0.5 d |
| S4 | users CRUD, temp password once, disable, reset | TestSuperAdminCreatesUser, TestAdminCannotManageUsers, TestDisableKillsSessions, TestLastSuperAdminProtected, TestResetForcesChange | 0.75 d |
| S5 | `/admin/stats` (peer merge), `/admin/audit`, purge on cleaner tick | TestStatsMergedTwoNodes, TestStatsCountsOnly, TestAuditFilters, TestSessionsPurged | 0.5 d |
| S6 | `portal/` shell: copied ui/layout/theme, login, guard, client, `REDLITE_PORTAL_DIR` | `npm run build` + `tsc --noEmit`, TestPortalDirServed (Go) | 1 d |
| S7 | pages: overview, projects, key dialog, sessions, users, audit | build + one Playwright smoke on compose if D10: login → create project → key shown once → revoke | 1.5 d |
| S8 | runbook: lock-down + break-glass in `doc/release.md`, compose `REDLITE_PORTAL_DIR`, DECISIONS D-051… | follow the runbook on a fresh `docker compose up` | 0.25 d |
Total ≈ 6.5 days. Persistence off → portal routes answer 503 "persistence required"; bearer routes work as today.

### Decisions for Devin (recommendation first)
- **D1 hashing**: Go bcrypt cost 12 + SQL for lockout/session/audit · alt pgcrypto `crypt` + `admin_sign_in()` (needs
  TLS or local DB and log hygiene; reverses `postgres_role.md:31`). Same hash format either way.
- **D2 sessions**: opaque HttpOnly cookie in `admin_sessions` · alt adv-style JWT + refresh.
- **D3 bootstrap gate**: admin key + latch row · alt also require `REDLITE_ADMIN_BOOTSTRAP=true`.
- **D4 serving**: Go serves `portal/dist` from `REDLITE_PORTAL_DIR` · alt Caddy/nginx in front (more moving parts) or
  `go:embed` (Node in every Go build).
- **D5 limiter**: `golang.org/x/time/rate` (new module, same x/ family as x/crypto, x/sync; clears the dependency
  checkpoint, DECISIONS.md:261) · alt a ~40-line hand-rolled bucket.
- **D6 trusted proxy flag**: yes, loopback-only · alt RemoteAddr only (per-IP limit is a no-op behind a proxy).
- **D7 roles**: 3 fixed · alt adv's roles + permissions tables. **D8 sheet export**: super_admin · alt admin.
- **D9 portal state**: React context · alt copy Redux authSlice. **D10 Playwright**: one smoke test · alt none.

## Sources and transcript claims
pgcrypto https://www.postgresql.org/docs/current/pgcrypto.html (trusted; bf max 72 bytes, default cost 6, max 31;
F.26.8.3 clear-text note) · RLS https://www.postgresql.org/docs/current/ddl-rowsecurity.html (owner bypass unless FORCE)
· SKIP LOCKED https://www.postgresql.org/docs/current/sql-select.html · https://pkg.go.dev/net/http (patterns,
MaxBytesReader, Cookie, CrossOriginProtection) · https://pkg.go.dev/crypto/subtle · https://pkg.go.dev/golang.org/x/time/rate
· code-heim: `go_30_gin_auth` (bcrypt + server sessions: the shape we take; gin, memstore, hard-coded secret and an
abort-without-return panic: not taken), `go_74_pgx` (pool + tx basics: already exceeded by `db/store.go`), `go_82_rate_limiting_revised`
(x/time/rate per IP: taken, plus the cap/sweep it lacks), `go_72_viper_config` (not taken: env-only config, DECISIONS.md:92).
Transcript claims (two Postgres transcripts as recorded in `doc/persist/postgres_role.md`; the third transcript was
not found in the repo):
- **pgcrypto auth: applies in part.** The table structure, lockout logic and session checks are SQL, but the hash
  compare stays in Go (D1).
- **SKIP LOCKED: does not apply.** The portal has no job queue; purge is a plain DELETE on the cleaner tick.
- **RLS: does not apply.** One app role owns the tables (the owner bypasses RLS), admin tables are not per tenant, and
  tenancy stays application-level (`design.md:35`).
