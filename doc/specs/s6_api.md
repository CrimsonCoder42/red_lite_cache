# S6 Management API (registry) — Spec

**Status:** built (2026-10-29); mounted and verified end to end in bootstrap wave 4. Tenancy routes (D-026…D-030) built 2026-10-29 (tenancy wave 1, agent B).
**Reference:** new. advo_cache had none: any `open` created any sheet (`advo_cache/sheet/manager.go:27-64`, K14). Design from `advo_cache/CACHE_PERSISTENCE_REPORT.md` Part 3 via `doc/persist/design.md` decisions 4–6.

## Goal

Let an operator give a second project its own sheet and API key without touching code, and replace the
O(n) `dump` client action with an authenticated export (K14, R11, R12).

## Acceptance criteria

1. `POST /admin/projects` with the admin key creates a project and returns its API key once; the key is stored only as SHA-256 with an 8-char prefix; the log line carries `key_prefix`, never the key.
2. `POST /admin/projects/{id}/sheets` with the admin key or that project's key creates a sheet with a bcrypt password that a reader can `open`; another project's key gets 404.
3. `GET /admin/sheets/{id}/export` with the admin key or the owning project's key returns every live entry with its remaining TTL; another project's key gets 404.
4. With `REDLITE_SHEET_AUTOCREATE=false`, `open` of a sheet the API did not create answers `sheet not found`; after the API creates it, a reader opens it.
5. Without `REDLITE_ADMIN_KEY`, every admin-key route answers 401.
6. Missing/invalid bodies → 400; duplicate project or sheet name → 409; unknown project → 404.
7. With persistence on, projects and keys are in PostgreSQL and a created sheet's row is written by the persist writer (OpCreate), so it survives a restart.
9. Tenancy (D-027…D-030): every route in the table below is admin-key only (a project key is 401). Writes are leader-only (503 `not leader` on a replica); listings and the session kill answer on any node. Suspend closes the project's sessions (4001) on every node and refuses `open` until resume; sheets and entries are kept. Revoking or rotating a key closes the sessions admitted with it (4002) on every node. A session kill closes one connection (4003) — on this node, else forwarded to the peer. Every mutation writes one `audit_events` row (persistence on) whose `detail` holds identifiers only.
8. `POST /admin/projects/{id}/revoke` (admin key only; 503 `not leader` on a replica; 404 unknown project) drops the project's sheets from memory (kicking collaborators) and from the writer's queue first, then revokes every key of the project and deletes its sheets (one transaction with persistence: `RevokeProjectKeys` + `DeleteProjectSheets`, entries cascade), drops them from the registry and kicks their collaborators (`sheet.Registry.Drop`), then NOTIFYs `project_revoked` (D-021). 204, no body. Without persistence (MemStore) it is local only.

## Routes (OpenAPI-ish)

Auth: `Authorization: Bearer <key>`. **A** = REDLITE_ADMIN_KEY only; **A|P** = admin key or that project's API key.
**L** = leader only (503 `{"error":"not leader"}` on a replica); **any** = answered on any node. Every error body is
`protocol.APIError` `{"error":"..."}`; 401 `unauthorized` for a missing, wrong or (on A routes) project key; 400 `invalid JSON`
for a malformed body. Paged listings take `?limit=` (default 50, max 200, larger is clamped; < 1 or non-integer → 400 `invalid limit`)
and `?cursor=` (opaque: base64url of `created_at|id`, the previous page's `next_cursor`; malformed → 400 `invalid cursor`).
Times are RFC 3339 UTC with nanoseconds. Types are in `protocol/admin.go`.

| Method | Path | Auth | Node | Body | 2xx response | Errors |
|---|---|---|---|---|---|---|
| POST | `/admin/projects` | A | L | `CreateProjectRequest{name}` | 201 `CreateProjectResponse` (api_key shown once) | 400 `name required`, 409 `project name taken` |
| GET | `/admin/projects` | A | any | — | 200 `Page[ProjectSummary]` (keys = live keys, sheets = sheets in this node's registry) | 400 limit/cursor |
| GET | `/admin/projects/{id}` | A | any | — | 200 `ProjectDetail` (every key incl. revoked, never a hash; sheets with live entry and this node's session counts) | 400 `invalid project id`, 404 `project not found` |
| POST | `/admin/projects/{id}/sheets` | A\|P | L | `CreateSheetRequest{name,password}` | 201 `CreateSheetResponse` | 400, 404 `project not found` (also another project's key), 409 `sheet name taken` |
| POST | `/admin/projects/{id}/keys` | A | L | optional `IssueKeyRequest{label}` (≤ 200 chars) | 201 `IssuedKey` (api_key shown once) | 400 `invalid project id` / `label too long`, 404 `project not found` |
| DELETE | `/admin/keys/{id}` | A | L | — | 204; sessions admitted with the key close 4002 `key revoked` on every node | 400 `invalid key id`, 404 `key not found` (unknown or already revoked) |
| POST | `/admin/keys/{id}/rotate` | A | L | optional `IssueKeyRequest{label}` ("" keeps the old label) | 201 `IssuedKey` — new key once; old key revoked in the same transaction, its sessions close 4002 | 400, 404 `key not found` (unknown or already revoked/rotated) |
| POST | `/admin/projects/{id}/suspend` | A | L | — | 204; every session of the project closes 4001 `suspended` on every node; `open` answers `suspended` + 4001 | 400, 404 `project not found` |
| POST | `/admin/projects/{id}/resume` | A | L | — | 204; opens admitted again on every node | 400, 404 |
| POST | `/admin/projects/{id}/revoke` | A | L | — | 204; keys revoked, sheets dropped (collaborators kicked), names never autocreated again | 400, 404 |
| GET | `/admin/sheets/{id}/export` | A\|P | any | — | 200 `SheetExport` | 404 `sheet not found` (also another project's key) |
| GET | `/admin/sessions` | A | any | — (`?project=<id>` filter, `?scope=local` = this node only) | 200 `Page[SessionInfo]`: this node's sessions + the peer's, each tagged `node`, ordered by (connected_at, session_id) | 400 `invalid project id`, limit/cursor |
| GET | `/admin/sheets/{name}/sessions` | A | any | — (`?scope=local`) | 200 `Page[SessionInfo]` of that sheet, local + peer | 400 limit/cursor |
| DELETE | `/admin/sessions/{id}` | A | any | — (`?scope=local` = do not forward) | 204; the connection closes 4003 `kicked by admin` within 1 s | 400 `invalid session id`, 404 `session not found` (neither node holds it), 502 `peer unreachable` / `peer answered <n>` |

**Peer.** The peer is `REDLITE_PEER_ADDRESS`, else (on a replica) `REDLITE_LEADER_ADDRESS`, converted ws→http (wss→https), path dropped.
It is called with this node's admin key (both nodes must share it), 2 s timeout, `?scope=local` so it never recurses; a listing pages
through at most 25 × 200 peer sessions. An unreachable peer is logged (Warn `API: peer sessions unavailable`) and the listing carries
this node's sessions only.

**Propagation.** Suspend, resume, key revoke/rotate: the leader updates its own `tenant.Directory` / `session.Registry`, then sends a
`protocol.ReplicaControl` frame to every connected replica (`Node.SendControl`) and, with persistence, also `NOTIFY redlite_control`
(`persist.Control` kinds `project_suspended`, `project_resumed`, `key_revoked`, `session_kill`). Both receivers run one apply
(`app.applyTenancy`); every apply is idempotent, so a replica that gets both does the same thing twice. With persistence the cleaner
tick also reconciles the suspended set from Postgres (a missed NOTIFY) and closes sessions of a project newly found suspended; boot
loads it before any listener exists.

**last_used_at.** `tenant.Directory.OnAdmit` → `api.Toucher.Touch`: at most once a minute per key (in-memory gate, pruned every
minute), handed without blocking to one goroutine that calls `Store.TouchKey` (`TouchAPIKeyUsed` with persistence). A full queue drops
the touch (Debug).

**Audit (criterion 11).** `persist.Auditor` with persistence (bounded queue of 1024, non-blocking, one goroutine on the persist ctx,
drained with a 2 s deadline at shutdown; Warn `Persist: audit dropped` at most once a second), `tenant.NopAuditor` without. Partitions
`audit_events_YYYY_MM` for this and next month (UTC) are created at boot and on every cleaner tick. Rows written by this API
(actor `admin`, or `key:<prefix>` for a project-key sheet create):

| action | subject | detail |
|---|---|---|
| project_created | project id | key_id, key_prefix |
| sheet_created | sheet name | sheet |
| project_revoked | project id | sheets_dropped |
| key_issued | key id | key_id, key_prefix |
| key_revoked | key id | key_id, sessions_closed |
| key_rotated | old key id | key_id, new_key_id, key_prefix (new), sessions_closed |
| project_suspended / project_resumed | project id | sessions_closed / — |
| session_kick | session id | session, sheet, node (written by the node that closed it) |

## Surface

| Kind | Name | Package |
|---|---|---|
| route | `POST /admin/projects`, `POST /admin/projects/{id}/sheets`, `GET /admin/sheets/{id}/export`, `POST /admin/projects/{id}/revoke` (revoke.go) | internal/api |
| func | `DropProject(reg, project, names) int`; `Revoker` (MemStore, DBStore) | internal/api |
| type | `CreateProjectRequest/Response`, `CreateSheetRequest/Response`, `SheetExport`, `ExportEntry`, `APIError` | protocol (admin.go) |
| func | `api.New(reg, store, adminKey, leader, api.Options{Tenants, Sessions, Auditor, Propagate, PeerURL})`, `(*Server).Register(mux)`, `NewMemStore()`, `NewDBStore(*db.Store)`, `NewToucher(store)` | internal/api |
| route | tenancy routes (tenancy.go), session routes (sessions.go) | internal/api |
| type | `Page[T]`, `IssueKeyRequest`, `IssuedKey`, `KeyInfo`, `SheetInfo`, `ProjectSummary`, `ProjectDetail`, `SessionInfo` | protocol (admin.go) |
| type/func | `Auditor` (`NewAuditor`, `Audit`, `Run`), `EnsureAuditPartitions`; `Control.KeyID/Session` + four tenancy kinds | internal/persist |
| func | `App.Auditor()`, `wireTenancy`, `propagate`, `applyTenancy`, `reconcileSuspended` | internal/app (wire_replica.go) |

## Value-source table

| Value | Source | Decided |
|---|---|---|
| admin key | `REDLITE_ADMIN_KEY` (config), constant-time compare | ✅ |
| project id | `uuid.New()` on create | ✅ |
| API key | `rlk_` + 32 bytes crypto/rand, base64url | ✅ |
| key at rest | sha256(key) → `api_keys.key_hash`; first 8 chars → `key_prefix` | ✅ design.md 4 |
| sheet password hash | bcrypt at `REDLITE_BCRYPT_COST` (sheet.Registry.Create) | ✅ design.md 5 |
| export entries | `sheet.Snapshot` under the sheet mutex; `expires_in_ms` = `Entry.Remaining` | ✅ |
| sheet → project | `sheet.ProjectID()` (registry; `sheets.project_id` when persisted) | ✅ D-013 |
| suspended_at | `SetProjectSuspended` (`COALESCE(suspended_at, now())`); MemStore: `now()` UTC µs | ✅ D-027 |
| key label | request body, ≤ 200 chars; rotate keeps the old one when "" | ✅ D-028 |
| last_used_at | `TouchAPIKeyUsed` after an admission, ≤ once/min/key | ✅ D-028 |
| cursor | base64url(`created_at` RFC3339Nano `\|` id) of the page's last row | ✅ D-029 |
| session rows | `session.Registry.List` (this node) + peer `GET …?scope=local`; `node` = registry's instance id | ✅ D-026 |
| audit node | instance id (`REDLITE_INSTANCE_ID` or generated) | ✅ 000002 |

## Failure behavior

| Failure | What the caller sees | What the system does |
|---|---|---|
| no/wrong key | 401 `{error:"unauthorized"}` | Warn `API: request refused` |
| other project's key | 404 (never reveals existence) | Warn |
| DB down (persist on) | 500 `{error:"internal error"}` | Error `API: <op> failed` (create project, list projects, get project, issue key, revoke key, rotate key, set suspended) / `API: key lookup failed` |
| write on a replica | 503 `not leader` | Warn `API: request refused` |
| peer down (listing) | 200, this node's sessions only | Warn `API: peer sessions unavailable` |
| peer down (kill) | 502 `peer unreachable` | Warn `API: peer kill failed` |
| NOTIFY fails | the route still succeeds (stream frame sent) | Error `API: control notify failed`; the cleaner reconcile covers suspend/resume |
| audit queue full / insert fails | nothing | Warn `Persist: audit dropped` / Error `Persist: audit write failed`, each ≤ 1/s with a count |

## Reuse

`sheet.Registry.Create` (K14 path), `sheet.Sheet.Snapshot`, `db.Store.ExecTx`, generated `db/sqlc/registry`.

## Decisions

D-013 (registry-owned lifecycle), D-015 (pgx/sqlc). ASSUMPTIONS: no rate limit on `/admin/*` yet (keys are 256-bit; brute force is impractical) — `doc/TODO.md`.

## Wire compatibility

New HTTP routes only. The WebSocket protocol is unchanged; `dump` answers an error naming the export route.

## Out of scope

Quotas (columns exist); a label on the first key of `POST /admin/projects` (`CreateProjectRequest` has no label field; issue a labelled key instead); replicating a MemStore's keys to a replica (persistence off: a promoted replica has no keys).

## Tests

| Criterion | Test | Kind |
|---|---|---|
| 1, 2, 3 | `internal/api/api_test.go` `TestProjectSheetExport`, `TestKeyAtRestIsHashed` | unit (httptest) |
| 4 | `testing/integration/api_test.go` `TestAutocreateOffRegistryPath` | integration |
| 5 | `TestEmptyAdminKeyRefusesEverything` | unit |
| 6 | `TestAuthAndValidation` | unit |
| 7 | `testing/integration/persist_test.go` (needs `REDLITE_TEST_DATABASE_URL`; SKIPPED here) | integration |
| 8 | `testing/integration/control_test.go` TestRevokeProjectMemStore, TestRevokeReplicaRefusesAdmin; TestRevokeEveryInstanceByNotify / ByReconcile (DB, skips) | integration |
| 9 (tenancy 3) | `testing/integration/tenancy_api_test.go` TestTenancyRegisterToOpen | integration |
| 9 (tenancy 4) | TestTenancySessionListing, TestTenancyPeerSessions | integration |
| 9 (tenancy 5, route) | TestTenancyKillSession, TestTenancyPeerSessions (forwarded kill) | integration |
| 9 (tenancy 6) | `testing/integration/suspend_test.go` TestSuspendLeaderAndReplica, TestSuspendControlKinds; TestSuspendPersisted (DB, skips) | integration |
| 9 (tenancy 7, 8) | TestTenancyRevokeOneKey, TestTenancyRotateKey | integration |
| 9 (paging, auth, replica) | TestTenancyPaging, TestTenancyAuth, TestTenancyReplicaWrites | integration |
| 9 (tenancy 11) | TestTenancyAuditRows (DB, skips) | integration |
