# Tenancy 2026-10-29 — work folder (THE BRIEF)

Request: `.claude/prompts/tenancy.md` (registration, admission, sessions, suspend/kick/revoke, peer auth).
Decisions D-024…D-031 are in `doc/DECISIONS.md` — made, do not re-decide. Anything you decide alone
goes in your report; the lead logs it in `doc/ASSUMPTIONS.md`. HIGH risk: a reviewer runs on the
whole diff with acceptance criteria 1–13 of tenancy.md.

## Baseline

`bash testing/run.sh` after wave 0: `Total | Passed | Failed | Skipped : 181 | 170 | 0 | 11`
(11 = persist, `REDLITE_TEST_DATABASE_URL` unset on this machine: DB tests must skip with the exact
string `SKIPPED: persist (no REDLITE_TEST_DATABASE_URL)`). Ports are now :8095/:8096 (Devin, commit 117c68e).

## What wave 0 (lead) already built — use it, do not rebuild it

| Piece | Where | Notes |
|---|---|---|
| `Request.APIKey` (`api_key`), `ErrMsgNotAuthorized` "not authorized", `ErrMsgSuspended` "suspended" | `protocol/messages.go` | additive; logger already masks `api_key` |
| Close codes `CloseSuspended=4001`, `CloseKeyRevoked=4002`, `CloseKicked=4003` + reasons | `protocol/messages.go` | terminal for the reader (D-030) |
| Control frames `project_suspended`, `project_resumed`, `key_revoked`, `session_kill` + `ReplicaControl` | `protocol/messages.go` | leader → replica when persistence is off |
| Admin shapes `Page[T]`, `IssueKeyRequest`, `IssuedKey`, `KeyInfo`, `SheetInfo`, `ProjectSummary`, `ProjectDetail`, `SessionInfo` | `protocol/admin.go` | |
| `Config.Admission` (`KeyAdmission()`), `Config.ClusterToken`, `IsProduction()`; autocreate default by env and forced off outside development; production without a token = config error | `internal/config/config.go` | tests: set `cfg.Environment`, `cfg.Admission`, `cfg.SheetAutocreate`, `cfg.ClusterToken` explicitly — derived defaults are computed only at Load |
| Migration 000002 (`projects.suspended_at`, `api_keys.last_used_at`/`label`, `audit_events` partitioned, `node` column) | `db/migration/000002_tenancy.*.sql` | immutable now |
| Queries `SetProjectSuspended`, `ListSuspendedProjects`, `ListProjectsPage`, `ListProjectKeys`, `GetAPIKey`, `RevokeAPIKey`, `TouchAPIKeyUsed`, `CreateAPIKey(label)`; `audit.InsertAuditEvent`, `ListAuditEvents`; `db.Store.Audit` | `db/query/*`, `db/sqlc/*`, `db/store.go` | need another query → report it, the lead adds it |
| `session.Registry` / `session.Session` (New, Touch, Sent, Kill once, List(Filter) ordered by connected_at,id, KillWhere) | `internal/session` | 2 tests |
| `tenant.Directory` (Admit, SetSuspended/Suspended/ReplaceSuspended, MarkRevoked/Revoked, OnAdmit hook), `tenant.KeyResolver`, `HashKey`, `Event` + `Auditor` + `NopAuditor`, `CheckBearer` | `internal/tenant` | 1 test |
| `api.Store` embeds `tenant.KeyResolver`; MemStore keeps full `Key` records; `ResolveKey` on Mem/DB | `internal/api/store.go` | |
| `App.Tenants()`, `App.Sessions()`, `App.Projects()` built in `app.New` before /ws | `internal/app/app.go` | |
| `replica.Options.ClusterToken` passed from config | `internal/replica/node.go`, `internal/app/wire_replica.go` | unused until agent C |
| `/health`: 200 `{"status":"ok"}` to anyone; role/epoch/instance only with the cluster token (or no token configured) | `internal/app/app.go` health | the peer probe must send the token |

## Owned paths

| Agent | Owns (edit only these) | Shared-file region |
|---|---|---|
| A | `internal/ws/**`, `internal/sheet/**`, `testing/integration/{admission,session}_test.go` | `internal/app/app.go`: ONLY the `ws.NewHandler(...)` call |
| B | `internal/api/**`, `internal/persist/**`, `internal/app/wire_replica.go`, `testing/integration/{tenancy_api,suspend}_test.go`, `doc/specs/s6_api.md` | `internal/app/app.go`: ONLY the `api.New(...)`/`adm.*` lines |
| C | `reader/**`, `internal/replica/**`, `cmd/redlite/**`, `testing/integration/{terminal,peerauth}_test.go`, `testing/integration/harness_server.go` (add, never change an existing signature), `doc/specs/{s3_reader,s4_replica}.md` | — |

Lead: protocol/, config, db/, tenant, session, docs, ledgers. Need a protocol field/query/config → report it.
Never `go get`/`go mod tidy`; never bind :8081/:8082; never a fixed port; never touch db/migration.

## CONTRACTS between agents

**A (ws)** — `ws.NewHandler(reg, locks, node, tenants *tenant.Directory, sessions *session.Registry, audit tenant.Auditor, opts)` (A picks the exact shape; update the one app.go call).
- open, after the existing gates: password mode keeps v1 behaviour exactly (criterion 12). Key mode: unknown sheet, wrong password, missing/foreign/revoked key → ONE reply `not authorized` then close 1008 — the same string for every cause (no oracle), Warn `WS: admission refused` with sheet, remote, reason (reason is for the log only). Suspended (either mode) → `SendAndClose` reply `suspended`, close 4001. Autocreate of a name `tenants.Revoked(name)` → refused.
- On join: `session.New(Params{Project: sh.ProjectID(), Sheet, KeyID, Remote, Epoch, Kill: <non-blocking close of this conn with that code+reason>})`, `sessions.Add`; on end `sessions.Remove`. `Touch(now)` on every inbound frame, `Sent()` on every outbound frame of that conn. Audit `session_open` / `session_close` (and `session_kick` is written by whoever kills — B for admin kills). `ws.Conn` gains an exported non-blocking `CloseWith(code websocket.StatusCode, reason string)` B and C can rely on through `session.Kill`.
- A sheet's collaborators are the sessions; kicking is `sessions.KillWhere(...)` — A does not need its own fan-out.

**B (api + persist + wire_replica)** — `api.New(...)` gains tenants, sessions, auditor, node (for control fan-out), peer address + admin key (peer listing). Endpoints D-027…D-029 + `DELETE /admin/sessions/{id}` (D-030): a session not on this node is forwarded to the peer's `DELETE /admin/sessions/{id}` with the admin key. Suspend/resume/key-revoke/kill: update the local `tenants`/`sessions`, write audit, then propagate: persistence on → `NOTIFY redlite_control` (extend `persist.Control` kinds and the listener's apply in wire_replica.go); persistence off → `node.SendControl(protocol.ReplicaControl{...})` (agent C adds it) and wire `replica.Options.OnControl` in wire_replica.go to apply on the replica. Boot: `tenants.ReplaceSuspended(ListSuspendedProjects)` with persistence. `tenants.OnAdmit` → `TouchAPIKeyUsed`, throttled in memory to once a minute per key, never blocking open. A persistent `tenant.Auditor` in internal/persist (bounded queue, non-blocking, own goroutine, ctx exit) plus audit partition creation (this + next month) at boot and on the cleaner tick; `NopAuditor` without persistence. GET /admin/sessions = local `sessions.List` + peer's (`?scope=local` on the forwarded call so it does not recurse), each row tagged `node`, paged.

**C (reader + replica + cmd)** — `reader.Config.APIKey` (sent as `api_key` on every open); terminal close codes 4001/4002/4003 → no reconnect, reader enters the closed state, `LastError() error` returns `ErrSuspended` / `ErrAdmitted` (4002, key revoked) / `ErrKicked` (new exported sentinels); `IsConnected()` false; other closes keep R1. An open refused `not authorized` behaves like today's auth failure (`ErrAuthFailed` from New). Replica: `/replica` checks `tenant.CheckBearer(r, opts.ClusterToken)` BEFORE `websocket.Accept` → 401, Warn `Replica: peer refused` with remote + reason only, zero snapshot bytes; the follower dials and the peer probe GETs with `Authorization: Bearer <token>`. `(*Node).SendControl(protocol.ReplicaControl)` (leader side, ALREADY WRITTEN by the lead in `leader.go`) and `Options.OnControl func(protocol.ReplicaControl)` (field ALREADY ADDED in `node.go`): C makes the follower decode the four control types (with the usual epoch check) and call `OnControl`. cmd/redlite: production with an empty token exits 1 with ONE Error line — a test proves it.

## Acceptance criteria → who proves it

| # | Criterion (tenancy.md) | Test owner |
|---|---|---|
| 1 | production: no key / wrong project's key refused; right key opens | A `admission_test.go` |
| 2 | production: unregistered sheet refused even with autocreate set | A (+ lead `internal/config` test) |
| 3 | POST project → POST sheet → reader opens with the returned key | B `tenancy_api_test.go` |
| 4 | GET /admin/sessions: project, sheet, remote, connected_at, last_seen advancing, node | B |
| 5 | DELETE /admin/sessions/{id} closes within 1 s; reader ErrKicked, no reconnect | B (route) + C (`terminal_test.go`) |
| 6 | suspend closes every session on leader AND replica, open refused, resume re-admits, entries intact | B `suspend_test.go` |
| 7 | DELETE /admin/keys/{id} closes only sessions on that key | B |
| 8 | rotate returns a new key once; old key refused | B |
| 9 | /replica without token → 401, no snapshot bytes; with → snapshot | C `peerauth_test.go` |
| 10 | production boot with empty token exits non-zero, one Error line | C (cmd) + lead config test |
| 11 | every admin mutation and session open/close/kick = one audit row (persist on), no secrets in detail | B (DB test, skips here) + A emits session rows |
| 12 | REDLITE_ADMISSION=password + autocreate: v1-shaped open works; TestV1CompatibilityFieldNames extended | A + lead (protocol test) |
| 13 | `bash testing/run.sh` green with -race | lead |
