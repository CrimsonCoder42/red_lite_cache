# S8 — Project sign-in with many sheets — Spec

**Status:** draft (2026-11-11). Nothing here is built.
**Reference:** new. Input: `doc/sheet_per_table_2026-11-11/REPORT.md`. Evidence:
`doc/project_sheets_2026-11-11/REPORT.md` (benchmarks, raw output in its `out/`).
**Companion:** `doc/specs/s9_ordered_structures.md` (the per-sheet structures).

_Slice id note: `doc/scope/SCOPE.md` already uses "S8" for Tenancy. This file is named as the
request asks (s8_project_sheets); SCOPE lists it as "PS"._

## Goal

A consumer signs in to its **project** once, with its API key, over a small pool of connections. It
then reads and writes any number of named **sheets** inside that project. Sheets have no password
and are unique by name within the project only, as in Google Sheets. It is a general feature for any
project. v1 single-sheet password opens keep working until they are retired.

## Decided (Devin, 2026-11-11; not reopened here)

- **Model:** project sign-in by API key; sheets live inside the project, have no password of their own, and their names are unique per project.
- **Lazy open:** the reader gets sheets by name and opens each on first use.
- **No namespaces:** namespaces inside one sheet are not the design.
- **One structure:** the server and the reader share the same structures (S9). The `patient` head index goes.
- **Portability:** the server will be ported to Rust; the wire protocol is the contract. Every frame below is specified for a second implementation.

---

## 1. Sign-in

**Today:** every connection opens one sheet. In key mode an open costs a key lookup (sha256,
`internal/tenant/tenant.go:113-145`) **plus** a bcrypt compare of the sheet password
(`internal/ws/admit.go:73`; `internal/sheet/sheet.go:106-109`). The measured cost was 431 ms to
connect 1 sheet vs 316 ms for 30 sheets at bcrypt cost 10, against 64 ms at cost 4
(`doc/sheet_per_table_2026-11-11/REPORT.md` §4). The bcrypt compares took the gain.

**New:**
- A connection's first frame is `open_project` (§6) carrying `api_key`.
- Admission is on the key alone. Today `Admit(ctx, project, apiKey)` needs the sheet's project and checks `owner != project` (`internal/tenant/tenant.go:113`, `:135`). It gains a key-only form, `AdmitKey(ctx, apiKey) (keyID, project, error)`, sharing the same checks:
  - the key resolves to the project by sha256 (D-020);
  - the key must not be revoked;
  - the project must not be suspended.
- **No bcrypt runs and no sheet password exists.** The API key replaces the per-sheet password check; authorization to a sheet is "the sheet belongs to the key's project".
- The admitted connection is a **project session** (D-026 sessions, now naming a project and a set of joined sheets).
- `project` in the frame is optional. When present it must equal the key's project id, else `not authorized` (no oracle, D-024).

**Coexistence with v1 (password mode):**

| First frame on a connection | Mode | What it can do |
|---|---|---|
| `open` (v1) | `REDLITE_ADMISSION=password`, or key mode with a legacy sheet that still has a hash | exactly today: one sheet, password checked by bcrypt, every frame without `sheet` |
| `open_project` (v2) | key mode (refused in password mode with `not authorized`) | §6: join any number of the project's sheets; every sheet frame names its `sheet` |

- **A connection never mixes the two.** `open_project` after `open` (or the reverse) answers `already in sheet` / `already in project`.
- **v1 `open` resolves the name within one project.** Because names are now per project, v1 resolves the name in the default project in password mode, or in the key's project in key mode with `api_key`. That stays unambiguous.
- **Retiring v1 mode (proposed; OPEN 6).** When all three conditions below hold, the v1 `open` path, `REDLITE_ADMISSION=password` and sheet password hashes are removed (a later D-id), and the `password_hash` column is dropped:
  1. `GET /admin/sessions` (D-029) has listed no `protocol_version: 1` session for 30 days. Sessions gain that field.
  2. backendAdvo runs on `reader.OpenProject` (the consumer list, step 3).
  3. No sheet still carries a password hash, or Devin accepts dropping them.

## 2. Project-scoped sheet names

**Today:** names are unique per node. `Registry.sheets` is a `map[string]*Sheet`
(`internal/sheet/registry.go:52`), and Postgres has `PRIMARY KEY (name)`
(`db/migration/000001_init.up.sql:58`).

**Name rule:** 1–128 bytes of `[A-Za-z0-9._-]`. It is checked on create and join, and violations get the error `invalid sheet name`. The rule keeps names safe in URLs, logs and Rust.

**Registry:**
- The key becomes `type SheetID struct{ Project uuid.UUID; Name string }`.
- `Get`, `Create`, `Restore`, `Remove` and `Drop` take a `SheetID`.
- `Sheets()` and `Stats()` are unchanged.
- `ProjectSheets(project)` lists one project's sheets (for limits and the admin list).

**Everything keyed by name today moves to `SheetID`:**

| Today | Where |
|---|---|
| Lock manager key `(sheet, key)` | `internal/lock/lock.go:21` |
| Revoked-name set | `internal/tenant/tenant.go:50`, `:178-192` |
| Session `Sheet` | `internal/session/session.go:23-30` |
| Persist ops (`op.Sheet` plus `op.ProjectID`, both already present) | `internal/persist/writer.go:250` |
| Replica frames (`sheet` plus `project_id` already sent on sheet frames) | `protocol/messages.go:189-200` |

**Persistence:**
- Migration `000003` is **proposed** in `doc/project_sheets_2026-11-11/000003_project_sheets.proposed.sql`:
  - `PRIMARY KEY (project_id, name)`;
  - `password_hash` nullable;
  - the name `CHECK` added `NOT VALID`, so legacy names stay.
- **Existing rows need no data move.** `UNIQUE (project_id, name)` already holds (`000001_init.up.sql:59`), and `cache_entries` already references `(project_id, sheet)` (`:70-71`).
- **`db/query` edits for Devin** (then `make sqlc`):
  - `UpsertSheet` changes `ON CONFLICT (name)` to `ON CONFLICT (project_id, name)` (`db/query/registry/registry.sql:29-33`);
  - `DeleteProjectSheets` already returns names per project (`:57-60`);
  - `ListSheets` adds `project_id` to its order.

**Admin API (S6):**

| Route | Change |
|---|---|
| `POST /admin/projects/{id}/sheets` | `password` becomes optional (only for a v1 sheet); the name rule is applied; `409 sheet name taken` is per project |
| `GET /admin/projects/{id}/sheets` | **new**: the project's sheets (name, entries, joined sessions) |
| `DELETE /admin/projects/{id}/sheets/{name}` | **new**: drops one sheet (the D-021 drop path for a single sheet) and kicks its joiners with `sheet_closed` |
| `GET /admin/projects/{id}/sheets/{name}/export` | replaces `GET /admin/sheets/{id}/export` (`internal/api/api.go:104`); the old route stays while names are globally unique, then answers 410 |
| `GET /admin/projects/{id}/sheets/{name}/sessions` | replaces `/admin/sheets/{name}/sessions` (`:116`) |

## 3. Sheet lifecycle

**Decision (recommended; OPEN 1): created on first use, inside the key's project, in every
environment.**
- **Trigger:** a `join` of an unknown name creates the sheet (OpCreate, replicated with `replicate_sheet`, persisted) and answers `created: true`.
- **Why this is safe in production when D-025 forbids autocreate:** D-025 guarded against *anonymous, global* names (K14: any client could take any name). A project sheet is created only by an admitted key, inside its own project, where the name collides with nothing.
- **Admin create remains** for setting per-sheet policy (S9) up front.
- **Lifetime:** sheets are still never evicted (D-042). They leave only by admin delete or project revoke.
- **Per-project limit:** `REDLITE_PROJECT_MAX_SHEETS`, default **256** (OPEN 2: the default is not measured; AdvoMedix needs about 59, `doc/sheet_per_table_2026-11-11/REPORT.md` §2.1).
  - A join that would create sheet 257 answers `sheet limit reached`; the connection stays open.
  - `0` = unbounded (development only, the same rule as autocreate today).
- **Revoke:** a revoked project drops every sheet (D-021, unchanged). A deleted sheet's name is free again in its project. The revoked-name set (D-025) applies to v1 password-mode autocreate only.

## 4. Connections: a small pool per project

**Measured** (`doc/project_sheets_2026-11-11/REPORT.md` §4, `BenchmarkPoolWidth`): acknowledged sets
from 32 goroutines over 30 sheets through k connections. The throughput knee is the pool default
(`pool` = the k where the next doubling adds under 10%):

| k connections | 1 | 2 | 4 | 8 | 16 | 30 |
|---|---|---|---|---|---|---|
| µs per acked set (1/throughput) | 13.45 | 8.43 | **7.55** | 7.61 | 7.96 | 8.58 |

So 1→2 is 1.60×, 2→4 adds 11.6% more, 4→8 nothing, and past 8 it gets slower (more connections to
serve). Knee = **4**.

**Rules:**
1. **Pool size** is `ProjectConfig.Pool`. Default **4**, the measured knee above. Range 1–16.
2. **Mapping:** sheet → connection `fnv1a32(name) mod pool`.
   - Stable across reconnects and across instances, so a sheet's frames always ride one connection, and its order (one connection, FIFO) is today's guarantee (D-014).
   - `Sheet(name, reader.Dedicated())` pins a hot sheet to its own extra connection (not counted in `Pool`).
3. **Every connection of a pool is its own project session.** Each sends `open_project` with `conn_index` (0..pool-1) and `pool_size`, for observability only; the server does not need them.
4. **A slow sheet cannot stall the others:**
   - **Server.** Writes never block: apply and broadcast enqueue without blocking, under the sheet's mutex only (D-014). Sheets have independent mutexes, so a hot sheet never holds another sheet's lock. A connection whose queue is full is closed (D-014 kick), which affects only the sheets mapped to it. They rejoin on reconnect; sheets on other connections continue.
   - **Reader.**
     - Sheets on other connections keep reading (local maps, R3 per connection).
     - A sheet whose connection is down misses until it rejoins.
     - A sheet that keeps getting its connection kicked can be pinned with `Dedicated()`.
   - **Order versus per-sheet queues.** Per-sheet sub-queues inside one connection were considered and rejected: they reorder frames of different sheets on the wire, which breaks the single-connection FIFO that §5's combined frames rely on. The pool plus pinning gives isolation without that.
5. **Read path:** unchanged: Gets are local (no connection involved). The pool only affects writes, invalidations and joins.

## 5. Order across sheets

**One project sequence (`pseq`).**
- Every applied write, expiry pass or multi-sheet clear in a project takes `pseq = project.pseq + 1`, assigned while holding the written sheet's mutex.
- `pseq` is an atomic counter per project; it never takes a lock of its own. So writes to different sheets still run in parallel. It is the 4.2× server gain measured in `doc/sheet_per_table_2026-11-11/REPORT.md` §4, which a project-wide mutex would give up.
- **Each sheet keeps its own `seq`** (R6, gap detection, D-014, unchanged).
- **Guarantee:** for any two writes, if one happens-before the other on the server, its `pseq` is smaller. Per sheet, `pseq` order = `seq` order.
- **Consumers of `pseq`:**
  - the replica stream (one total order per project);
  - persistence;
  - the audit log;
  - a reader that wants a high-water mark (`Project.Seq()`).

**Multi-sheet clear (one message).** `clear` (§6) carries up to 64 groups `{sheet, key | prefix | suffix}` (OPEN 3: the cap). Groups may name any of the project's sheets, joined or not.

The server applies it as one unit:
1. Lock every named sheet's mutex in **ascending (project, name) order**. This is the one lock order; no other path holds two sheet mutexes.
2. Apply every group. Each touched sheet gets `seq + 1` and a sweep-log entry (R6).
3. Take **one** `pseq` for the whole clear.
4. For every connection that joined any touched sheet, enqueue **one** `invalidate` frame carrying that connection's parts (§6). It goes under the held mutexes, so it lands in each sheet's FIFO position.
5. Observe one OpClear for replication and persistence.
6. Unlock, then reply to the sender with the per-sheet seqs and deletes.

**What other instances observe.**
- Parts riding the same connection arrive in one frame and are applied together. With the default mapping, sheets on different connections get their parts in separate frames, at most one round trip apart.
- **The reader never serves a pre-clear value after it applied the part for that sheet**, and R6 refuses any read-through set based on a seq older than the part.
- Proposed (OPEN 8): readers do not get cross-connection atomic visibility; that would need a reorder buffer keyed by `pseq` with gap detection over sheets the reader never joined.

**R2 (replay) across sheets.**
- **One queue per project:** the reader's delete queue holds ops tagged with their sheet (or the groups of a `clear`), in issue order.
- **Disconnected:** a delete applies locally first (D-005) and is queued.
- **Reconnect replay:** on reconnect of connection c, the queue is replayed **in order**:
  - single-sheet ops of sheets mapped to c are sent on c;
  - a queued `clear` is sent once, on the lowest-numbered live connection; the server accepts groups for any project sheet on any project connection.
- **Readiness:** a sheet becomes readable again only when no queued op naming it remains (the D-005 rule, per sheet).
- **Overflow** (`MaxPendingDeletes`) collapses the queue to one `clear` with `prefix ""` for **every sheet the queue named** (D-005 widened to the sheets involved; other sheets keep their maps).

**R6 (`base_seq`) across sheets.**
- Per sheet, as today: the reader's miss record is keyed `(sheet, key)` and holds that sheet's `seq`. The server's sweep log is per sheet (`internal/sheet/sweeplog.go`).
- A `clear` writes one sweep-log entry in each touched sheet. A read-through `set` in any of them with an older `base_seq` is refused with `stale write`.
- No cross-sheet state is needed.

## 6. Wire protocol (v2, additive)

All frames are JSON text over WebSocket at `/ws`.
- **Frame kinds:** client frames carry `action`, broadcasts carry `type`, replies carry `success` and echo `id`.
- **Version:** v2 frames are valid only after `open_project` with `version: 2`.
- **Unknown fields:** must be ignored by both sides.
- **Strings:** UTF-8; key comparison and sort order are by **bytes**.
- **Sequence numbers:** `seq` and `pseq` are unsigned 64-bit.
- **Epoch (D-012):** every request may carry `epoch`.

### Client → server

| action | fields (required **bold**) | reply |
|---|---|---|
| `open_project` | **`version`**=2, **`api_key`**, `project` (uuid), `conn_index`, `pool_size`, `epoch`, `id` | `{success, id, protocol_version:2, project_id, epoch, pseq}`. Refusals: `not authorized` + close 1008; `suspended` + close 4001; `not leader`; `stale epoch` |
| `join` | **`sheet`**, `id`, `epoch` | `{success, id, sheet, created, seq, pseq}` then **`sheet_state`** frame(s) for that sheet. Refusals: `invalid sheet name`, `sheet limit reached`, `sheet not found` (only when lifecycle is admin-only, OPEN 1) |
| `leave` | **`sheet`**, `id` | `{success, id, sheet}`; no more frames for that sheet |
| `get` | **`sheet`**, **`key`**, `id` | as v1, plus `sheet`. `not joined` when the sheet is not joined on this connection |
| `set` / `set_null` | **`sheet`**, **`key`**, `value`, `ttl_ms`, `base_seq`, `id`, `epoch` | `{success, id, sheet, key, seq, pseq, expires_in_ms}`. `ttl_ms` replaces v1 `ttl` (minutes; OPEN 9): 0/absent = the sheet's policy default (S9 §4) |
| `delete` / `delete_prefix` / `delete_suffix` | **`sheet`**, `key` / `prefix` / `suffix`, `id`, `epoch` | `{success, id, sheet, deleted, seq, pseq}` |
| `clear` | **`groups`**: array of `{sheet, key}`, `{sheet, prefix}` or `{sheet, suffix}` (exactly one of the three; `prefix:""` = whole sheet), `id`, `epoch` | `{success, id, pseq, parts:[{sheet, seq, deleted}]}`. Refused whole (nothing applied): `invalid sheet name`, `sheet not found`, `too many groups` |
| `lock` / `unlock` | **`sheet`**, **`key`**, `lock_ttl`, `id` | as v1, scoped to `(project, sheet, key)` |

`set`, `delete*` and `clear` may name a sheet not joined on this connection (a writer need not hold
a sheet's map). `get` requires a join: it reads the server copy.

### Server → client

| type | fields | when |
|---|---|---|
| `sheet_state` | `sheet`, `seq`, `pseq`, `epoch`, `chunk` (0-based), `last` (bool), **`entries`**: array of `[key, value, expires_in_ms]` (`expires_in_ms` 0 = none) | after a join. Entries are **sorted ascending by key bytes across all chunks of one join** (S9 §3). A chunk is at most `REDLITE_STATE_CHUNK_BYTES` (OPEN 4; proposed 1 MiB) |
| `replicate` | `sheet`, `key`, `value`, `seq`, `pseq`, `expires_in_ms` | a set by another session |
| `invalidate` | either `sheet` + exactly one of `key`, `keys`, `prefix`, `suffix` + `seq`; **or** `parts`: array of `{sheet, seq, key \| prefix \| suffix}` (a multi-sheet `clear`); and `pseq` | a delete, an expiry batch (`keys`), or a clear |
| `sheet_closed` | `sheet`, `reason` (`deleted`, `revoked`) | the sheet left the project; the reader drops its map and its handle errors with `ErrSheetClosed` |

- **Sender exclusion:** the session that sent a write does not get its broadcast; its reply carries `seq`/`pseq`, as in v1.
- **Gap detection:** a reader that sees a `seq` gap for a sheet (seq ≠ last + 1) rejoins that sheet.

### Leader → replica

- **Every existing frame** (`wire.md` § Leader → replica) adds `pseq` and keeps `project_id` with `sheet`.
- **New:** `replicate_clear` `{project_id, pseq, parts:[{sheet, seq, key|prefix|suffix}], epoch}` and `replicate_sheet_deleted` `{project_id, sheet, epoch}`.
- `snapshot_sheet` carries `entries` sorted as in `sheet_state`.

### Errors (new strings)

- `already in project`
- `invalid sheet name`
- `sheet limit reached`
- `not joined`
- `too many groups`
- `sheet required` (a v2 frame without `sheet`)

`wire.md` gains a "v2: projects" section with the tables above **in the build step that ships them**
(§ Build plan). This spec is the draft.

## 7. Reader API

```go
p, err := reader.OpenProject(reader.ProjectConfig{
    LeaderAddr, BackupAddr string
    APIKey     string
    Project    string        // optional uuid check
    Pool       int           // default 4 (§4)
    MaxEntries int           // per sheet, default reader.DefaultMaxEntries
    // ConnectTimeout, WriteTimeout, MaxPendingDeletes, Reconnect*, MissWindow, Logger, Context: as Config
})
s := p.Sheet("orders")                // cheap handle, cached by name; joins on first use
ok := s.Get("k", &v)                  // local; a not-yet-joined sheet starts its join and misses
err = s.Set(ctx, "k", v)              // waits for the join (bounded by WriteTimeout), then the ack
err = s.Delete(ctx, "k"); s.DeletePrefix(ctx, p); s.DeleteSuffix(ctx, x); s.Has; s.IsNull; s.Stats()
err = p.Clear(ctx, reader.Prefix("orders", "customer:42:"), reader.Prefix("invoices", "customer:42:"))
p.Seq(); p.IsConnected(); p.Stats(); p.Close(); p.LastError()
```

- **Errors:**
  - unchanged sentinels (`ErrNotConnected`, `ErrClosed`, `ErrAuthFailed`, `ErrStale`, `ErrTimeout`, `ErrSuspended`, `ErrAdmitted`, `ErrKicked`);
  - new `ErrSheetClosed` and `ErrSheetLimit`.
- **`IsConnected`** is true when every pool connection is up. `s.IsConnected()` is per sheet (its connection, and its join done).
- **`reader.New(Config)`** stays: one v1 single-sheet connection, exactly today.
- **`reader/v1compat` is unchanged.** It wraps `reader.New` and sends the v1 `open{sheet, password}`, which §1 keeps. backendAdvo's cut-over step 1 (an import-path change, D-035) still works against a server running this spec. v1compat gains no project surface. A consumer that wants projects calls `reader.OpenProject` directly (cut-over step 3, below).

## 8. R1–R12

| R | Still met? How | Test that proves it (existing → new) |
|---|---|---|
| R1 reconnect forever | each pool connection runs today's backoff loop (D-004) independently | `reader` `TestR1ReconnectForever` → new `TestProjectR1EachConnReconnects` |
| R2 deletes queued and replayed | one project queue, per-sheet readiness, `clear` replayed once (§5) | `TestR2DeletesQueuedAndReplayed` → new `TestProjectR2ReplayAcrossSheets`, `TestProjectR2ClearReplayedOnce`, `TestProjectR2OverflowWipesOnlyNamedSheets` |
| R3 disconnected = miss | per sheet: a sheet whose connection is down, or that is not joined yet, misses | `TestR3DisconnectedReadsMiss` → new `TestProjectPoolOneConnDown` |
| R4 TTL + bound | `MaxEntries` per sheet; expiry per S9 §4 | `TestR4ExpiryAndBound` → new `TestProjectR4BoundPerSheet` |
| R5 server expiry reaches readers | `invalidate{sheet, keys}` per sheet | `TestKeyLoop` → new `TestProjectR5ExpiryBroadcastPerSheet` (wire) |
| R6 no re-cache of pre-write rows | per-sheet `base_seq`; `clear` logs every touched sheet | `TestRaceLoop` → new `TestProjectR6ClearRefusesOlderBase` (wire) |
| R7 no full scan per delete | S9 ordered index, O(log n + matches) | `TestLocalIndexMatchesScan` → S9 `TestGroupsMatchNaive` + benchmark targets |
| R8 typed, isolated Get | unchanged in the Sheet handle | `TestR8TypedIsolatedGet` → new `TestProjectR8` |
| R9 acked writes | every v2 write is acked with `seq` and `pseq` | `TestR9AckedSet` → new `TestProjectR9` (wire) |
| R10 boot while down | `OpenProject` returns after `ConnectTimeout`, still reconnecting | `TestR10NewWhileDown` → new `TestProjectR10` |
| R11 no collisions | names unique per project; two projects may share a name | `TestAutocreateOffRegistryPath` → new `TestProjectSameNameTwoProjects` (wire) |
| R12 no shared literal credential | no sheet password; the API key is per project, hashed at rest (D-020), revocable per key (D-028) | `TestKeyAtRestIsHashed` → new `TestProjectNoSheetPassword` (wire) |

## Acceptance criteria

Each criterion is verifiable against a running server (**W** = black-box over the wire, any language)
or a `-race` test (**R**).

1. **(W)** `open_project` with a live key answers `success` with the key's `project_id`. It answers `not authorized` + close 1008 for a missing, unknown, revoked or other-project key (`project` mismatch). It answers `suspended` + close 4001 for a suspended project. No bcrypt runs (asserted by a server counter `bcrypt_checks` staying 0 in `/health` stats, development only).
2. **(W)** Two projects each `join` a sheet named `pulse`; a set in one is never visible in the other.
3. **(W)** `join` of an unknown valid name creates it (`created:true`, `seq:0`). The 257th sheet of a project with the default limit answers `sheet limit reached`. An invalid name answers `invalid sheet name`.
4. **(W)** `sheet_state` entries arrive sorted by key bytes across all chunks, and the chunks of one join concatenate to exactly the sheet's live keys.
5. **(W)** A set in sheet A broadcasts `replicate{sheet:"A"}` to every other session joined to A, and nothing to sessions joined only to B.
6. **(W)** `pseq` increases by exactly 1 per applied write across all sheets of a project. Per sheet, `seq` increases by 1 and in the same order as `pseq`.
7. **(W)** `clear` with groups in 3 sheets applies all or nothing (an invalid group → nothing applied, `seq`s unchanged). It takes one `pseq`, and a session joined to all 3 sheets on one connection receives exactly one `invalidate{parts}` frame.
8. **(W)** After a `clear` touching sheet B, a `set` in B with a `base_seq` older than B's new `seq` is refused `stale write`.
9. **(R)** With pool=2 and one connection cut at a proxy, sheets on the other connection keep serving Gets and acked writes. Sheets on the cut connection miss until they rejoin.
10. **(R)** Deletes issued while every connection is down, then reconnect: each sheet becomes readable only after its queued ops are acked. A queued `clear` is sent exactly once.
11. **(R)** `reader/v1compat` against a server with this spec passes the existing conformance suite (`testing/conformance`, both admission modes) unchanged.
12. **(R)** A slow consumer on connection 0 that gets kicked does not delay acks or broadcasts on connection 1 (p99 of acks on conn 1 within 2× of an unloaded run).
13. **(W)** `DELETE /admin/projects/{id}/sheets/{name}` sends `sheet_closed` to every joiner and the name can be joined (recreated) afterwards.
14. **(R)** Persistence on: a restart hydrates sheets by `(project, name)`. Two projects with the same sheet name both come back. (Postgres-gated, as `internal/persist` today.)
15. **(R)** Replica: a promoted replica serves `open_project` and the same `(project, name)` sheets with the same `seq` and `pseq`.

## Tests that must exist

| Test | Criterion | Black-box over the wire? | Suite |
|---|---|---|---|
| `TestProjectOpenAdmission` | 1 | **yes** | testing/integration (+ portable list) |
| `TestProjectSameNameTwoProjects` | 2, R11 | **yes** | integration |
| `TestProjectJoinCreatesAndLimits` | 3 | **yes** | integration |
| `TestProjectSheetStateSortedChunks` | 4 | **yes** | integration |
| `TestProjectBroadcastScopedToSheet` | 5 | **yes** | integration |
| `TestProjectPseqOrder` | 6 | **yes** | integration |
| `TestProjectClearAtomic` | 7 | **yes** | integration |
| `TestProjectR6ClearRefusesOlderBase` | 8, R6 | **yes** | integration |
| `TestProjectPoolOneConnDown` | 9, R3 | no (proxy + reader) | integration |
| `TestProjectR2ReplayAcrossSheets`, `…ClearReplayedOnce`, `…OverflowWipesOnlyNamedSheets` | 10, R2 | no | reader + integration |
| `testing/conformance` both modes | 11 | no | conformance |
| `TestProjectSlowConnIsolated` | 12 | no | stress |
| `TestProjectSheetDelete` | 13 | **yes** | integration |
| `TestProjectHydrateByProject` | 14 | no | persist (Postgres-gated) |
| `TestProjectR1EachConnReconnects` | R1 | no (proxy + reader) | reader + integration |
| `TestProjectR4BoundPerSheet` | R4 | no | reader |
| `TestProjectR5ExpiryBroadcastPerSheet` | R5 | **yes** | integration |
| `TestProjectR8` | R8 | no | reader |
| `TestProjectR9` | R9 | **yes** | integration |
| `TestProjectR10` | R10 | no | reader |
| `TestProjectNoSheetPassword` | R12 | **yes** | integration |
| `TestProjectReplicaPromotes` | 15 | no | integration |
| leaks `TestL14ProjectSheetsDrain` | — | no | leaks: join 30 sheets, leave, goroutines and buckets back to zero |

The **yes** rows only open sockets at an address. They are listed in a new
`testing/conformance/wire_v2.md`, so the Rust server can run them unchanged by pointing them at its
address.

## Benchmark targets (Go build now, Rust port later)

From `doc/project_sheets_2026-11-11/REPORT.md` and `doc/sheet_per_table_2026-11-11/REPORT.md` (M1
Pro, loopback, medians of 6):
- **Connect, 30 sheets / 100k keys:** at most the v1 cost-4 figure of **64 ms** (no bcrypt left), measured over 1 pool connection plus joins.
- **Acked write throughput, 30 sheets, 32 goroutines, pool 4:** at most **7.55 µs per acked set** (`BenchmarkPoolWidth/conns=4`, report §4); pool 1 must stay at most 13.45 µs.
- **Server apply, 32 parallel writers over 30 sheets:** at most **301 ns/op**. `pseq` must not regress it more than 10%.
- **Parallel local Get:** within 10% of today's **~150 ns** (report §2).

## Value-source table

| Value | Source | Decided |
|---|---|---|
| project of a connection | the API key's row (`api_keys.project_id`) via `tenant.KeyResolver` | ✅ |
| sheet name rule | this spec §2 | ❌ OPEN 5 |
| `REDLITE_PROJECT_MAX_SHEETS` default 256 | not measured | ❌ OPEN 2 |
| pool default 4 | `BenchmarkPoolWidth` knee (report §4); measured, awaiting OPEN 7 | ❌ OPEN 7 |
| `clear` group cap 64 | not measured | ❌ OPEN 3 |
| `REDLITE_STATE_CHUNK_BYTES` 1 MiB | equals `REDLITE_MAX_FRAME_BYTES` default (`internal/config/config.go:136`) | ❌ OPEN 4 |
| `pseq` | per-project counter, in memory; replicated in every frame; persisted as `projects.pseq` at flush | ✅ |
| `ttl_ms` default | the sheet's policy (S9 §4), else `REDLITE_CACHE_TTL` | ✅ source; the unit change is OPEN 9 |

## Failure behaviour

| Failure | What the caller sees | What the system does |
|---|---|---|
| Socket down (one of the pool) | its sheets miss (R3); writes on them return `ErrNotConnected` (deletes queue, R2) | that connection reconnects; other connections untouched |
| Sheet not joined | `Get` misses and starts the join; `Set` waits for the join up to `WriteTimeout` | `join`, then `sheet_state` |
| Lock held | as v1 | locks keyed `(project, sheet, key)` |
| Persist unavailable | nothing (write-behind) | as S5 |
| Replica lag | as v1 | frames carry `pseq`; a promoted replica continues the project's `pseq` |
| Sheet limit | `ErrSheetLimit` from the handle's first write; Gets miss | nothing created |
| Sheet deleted by admin | `ErrSheetClosed` | `sheet_closed` to joiners |

## Reuse

- Admission: `tenant.Directory.Admit` (`internal/tenant/tenant.go:113`).
- Sessions: `session.Registry` (`internal/session/session.go:117-182`).
- Apply + broadcast under the sheet mutex: `Sheet.Apply` (`internal/sheet/sheet.go:267`).
- Bounded per-conn queue and kick: `internal/ws/conn.go:104`.
- Reader backoff, replay, miss records: `reader/conn.go`, `reader/reader.go`, reused per pool connection.
- Proxy for tests: `testing/integration/harness.go:175`.

## Decisions

- **Governing:** D-012 (epoch), D-014 (per-sheet order, bounded queues), D-020 (key hashing), D-021/D-027/D-028/D-030 (revoke, suspend, kick), D-024 (admission), D-026 (sessions), D-042 (never evicted).
- **Proposed new D-ids:** to be numbered by Devin. The CHANGELOG already earmarked a D-043 elsewhere (`doc/reader_optimize_2026-11-10/report.md` FOUND).
  - project sheets replace sheet passwords;
  - per-project names;
  - create-on-join;
  - pool + fnv mapping;
  - `pseq`;
  - `clear`;
  - v1 retirement rule.

## Wire compatibility

Additive. v1 frames and the v1 compatibility list (`doc/protocol/wire.md` § v1 compatibility list) are
unchanged. v2 adds new actions and types and reuses v1 field names with the same meaning. **One
semantic change for v1:** in key mode the sheet in `open` is resolved within the key's project (today:
globally). Every name is unique globally today, so no existing open changes outcome until two
projects share a name.

## Out of scope

- Cross-connection atomic visibility of a `clear` (§5).
- Per-sheet sub-queues inside a connection (§4.4).
- Sheet rename.
- Sharing a sheet between projects.
- Per-sheet ACLs inside a project (every key of a project sees every sheet).

## Build plan (each step ships and tests alone)

1. **Per-project identity, no wire change.**
   - Registry key `SheetID`; locks, sessions, revoked names and replica keyed by it.
   - Migration 000003 and the db/query edits (Devin runs sqlc); v1 `open` resolves within the project.
   - Tests: `TestProjectSameNameTwoProjects` via admin create + v1 open with api_key; `TestProjectHydrateByProject`.
2. **`open_project` + `join`/`leave` + v2 single-sheet frames**, one connection.
   - `wire.md` v2 section; `protocol` structs + `TestV2FieldNames`.
   - Tests: criteria 1, 2, 3 (create and name rule; the limit comes in step 7), 5, 13, and 11 (conformance through v1compat, re-run in every later step too).
3. **`pseq`**, in every frame and replica frame; persisted per project. Tests: criteria 6, 15.
4. **Sorted, chunked `sheet_state`** (needs S9 step 2's sorted export). Test: criterion 4.
5. **`clear`**: multi-sheet apply, lock order, `invalidate{parts}`, `replicate_clear`. Tests: criteria 7, 8.
6. **Reader `OpenProject`/`Sheet`**, pool of N with fnv mapping, project replay queue. Tests: criteria 9, 10, 12; R1–R10 project variants.
7. **Admin routes** (list, delete, per-project export/sessions) and `REDLITE_PROJECT_MAX_SHEETS` in `internal/config`. Test: criterion 3 limit.
8. **v1 retirement** when the §1 rule holds (separate D-id).

## What backendAdvo changes (list; no edits there)

1. **Today it runs advo_cache** (`backendAdvo/go.mod:6`, `:86`). Cut-over step 1 (D-035) is unchanged: import `reader/v1compat`, still one sheet with a password.
2. **Add `CACHE_API_KEY`** (and an optional `CACHE_PROJECT`) to `backendAdvo/util/config.go:20-23`; drop `CACHE_PASSWORD` and `CACHE_SHEET` after step 3.
3. **`cache.NewStore`** (`backendAdvo/cache/store.go:127-138`): `reader.OpenProject(...)` once per process, and one `p.Sheet(<table>)` handle per cache file, by the table → sheet list in `doc/sheet_per_table_2026-11-11/REPORT.md` §2.2.
4. **Cascades** (`medical_episode.go:93-162`, `medical_condition.go:178-244`, `store.go:597` `InvalidateAllAnalytics` (prefix list at `:583`), the fitness and user cascades) become **one `p.Clear(...)`** each, listing every sheet group. That replaces up to 13 + 10E separate sweeps with one ordered, atomic message.
5. **Keys drop their root segment** inside a sheet (`pulse:patient:<pid>:…` → `patient:<pid>:…` in sheet `pulse`). A mixed key (§2.3 of the input report) picks one home sheet.
6. **TTL constants become sheet policy** (S9 §4) or stay per set as `ttl_ms`.
7. **Deploy:** the password literal in `app.env:20` and `deploy-backendadvo-production.yml:142` goes; the API key comes from a secret.
8. **`/health`** (`api/routes/router.go:131`) uses `p.IsConnected()` (all connections) or a chosen sheet subset.

## OPEN DECISIONS FOR DEVIN

1. Create sheets on first `join` in every environment, bounded per project (instead of admin-only in production)? **yes / no**
2. `REDLITE_PROJECT_MAX_SHEETS` default 256? **yes / no**
3. Cap a `clear` at 64 groups? **yes / no**
4. Chunk `sheet_state` at 1 MiB (`REDLITE_STATE_CHUNK_BYTES`)? **yes / no**
5. Sheet name rule `[A-Za-z0-9._-]{1,128}`? **yes / no**
6. Retire v1 password mode by the §1 rule (30 days without a v1 session + backendAdvo on `OpenProject`)? **yes / no**
7. Default pool size 4 (the measured knee, §4), with `Dedicated()` pinning? **yes / no**
8. Accept per-connection (not cross-connection) atomic visibility for a multi-sheet `clear` on other readers? **yes / no**
9. v2 `set` takes `ttl_ms` (milliseconds) instead of v1's minutes? **yes / no**
