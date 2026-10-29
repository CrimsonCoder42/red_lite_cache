# S6 Management API (registry) — Spec

**Status:** built (2026-10-29); mounted and verified end to end in bootstrap wave 4
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
8. `POST /admin/projects/{id}/revoke` (admin key only; 503 `not leader` on a replica; 404 unknown project) drops the project's sheets from memory (kicking collaborators) and from the writer's queue first, then revokes every key of the project and deletes its sheets (one transaction with persistence: `RevokeProjectKeys` + `DeleteProjectSheets`, entries cascade), drops them from the registry and kicks their collaborators (`sheet.Registry.Drop`), then NOTIFYs `project_revoked` (D-021). 204, no body. Without persistence (MemStore) it is local only.

## Surface

| Kind | Name | Package |
|---|---|---|
| route | `POST /admin/projects`, `POST /admin/projects/{id}/sheets`, `GET /admin/sheets/{id}/export`, `POST /admin/projects/{id}/revoke` (revoke.go) | internal/api |
| func | `DropProject(reg, project, names) int`; `Revoker` (MemStore, DBStore) | internal/api |
| type | `CreateProjectRequest/Response`, `CreateSheetRequest/Response`, `SheetExport`, `ExportEntry`, `APIError` | protocol (admin.go) |
| func | `api.New(reg, store, adminKey)`, `(*Server).Register(mux)`, `NewMemStore()`, `NewDBStore(*db.Store)` | internal/api |

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

## Failure behavior

| Failure | What the caller sees | What the system does |
|---|---|---|
| no/wrong key | 401 `{error:"unauthorized"}` | Warn `API: request refused` |
| other project's key | 404 (never reveals existence) | Warn |
| DB down (persist on) | 500 `{error:"internal error"}` | Error `API: create project failed` / `API: key lookup failed` |

## Reuse

`sheet.Registry.Create` (K14 path), `sheet.Sheet.Snapshot`, `db.Store.ExecTx`, generated `db/sqlc/registry`.

## Decisions

D-013 (registry-owned lifecycle), D-015 (pgx/sqlc). ASSUMPTIONS: no rate limit on `/admin/*` yet (keys are 256-bit; brute force is impractical) — `doc/TODO.md`.

## Wire compatibility

New HTTP routes only. The WebSocket protocol is unchanged; `dump` answers an error naming the export route.

## Out of scope

Key rotation and single-key revocation (project-wide revoke is built), quotas (columns exist), listing projects.

## Tests

| Criterion | Test | Kind |
|---|---|---|
| 1, 2, 3 | `internal/api/api_test.go` `TestProjectSheetExport`, `TestKeyAtRestIsHashed` | unit (httptest) |
| 4 | `testing/integration/api_test.go` `TestAutocreateOffRegistryPath` | integration |
| 5 | `TestEmptyAdminKeyRefusesEverything` | unit |
| 6 | `TestAuthAndValidation` | unit |
| 7 | `testing/integration/persist_test.go` (needs `REDLITE_TEST_DATABASE_URL`; SKIPPED here) | integration |
| 8 | `testing/integration/control_test.go` TestRevokeProjectMemStore, TestRevokeReplicaRefusesAdmin; TestRevokeEveryInstanceByNotify / ByReconcile (DB, skips) | integration |
