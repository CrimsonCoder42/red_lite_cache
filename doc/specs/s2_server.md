# S2 Server (ws, lock, app, cmd) — Spec

**Status:** built
**Reference:** `advo_cache/ws/handler.go:76-436` (routing, actions), `advo_cache/lock/lock.go:34-175` (locks), `advo_cache/main.go:20-262` (wiring)

## Goal

The leader's client surface: `/ws` with every action in `doc/protocol/wire.md`, connection-owned
distributed locks, `/health`, and one `app.App` that `cmd/redlite` and the integration harness both run.
Consumer: backendAdvo through `reader/` (R1–R12) and any v1 reader still pinned.

## Acceptance criteria

1. KEY LOOP: a set on reader A reaches reader B; a delete removes it on B; a ttl-0 set expires after `REDLITE_CACHE_TTL` on every reader and the janitor broadcasts `invalidate{keys,seq}`; `set_null` → `IsNull` true, `Get` miss (R4, R5). — `TestKeyLoop`
2. SWEEP LOOP: `delete_prefix` / `delete_suffix` drop exactly the matching keys on every reader and the server; replies `{success,id,deleted,seq}`, broadcasts carry `prefix`/`suffix` even when `""`. — `TestSweepLoopPrefix`, `TestSweepLoopSuffix`, `TestSweepReplyShape`
3. LOCK LOOP: contention → `lock held by another client` with `key`; owner disconnect releases; `lock_ttl` is seconds and an expired lock is takeable; unlock of a lock now held by another → `not lock owner`; unlock of none → `lock not found`; owner re-lock refreshes TTL; locks are per sheet. — `TestLockLoop`, `TestLockRelockRefresh`, `internal/lock` unit tests
4. Any action before `open` → `must open sheet first`, echoing `id`. — `TestEdgeRouting/action_before_open`
5. Wrong password → `invalid password` reply, then the socket closes (1008). — `TestEdgeRouting/wrong_password…`
6. Empty sheet or password → `sheet and password required`; the conn stays usable. — `TestEdgeRouting/empty_sheet_or_password`
7. Malformed / mistyped JSON → `invalid JSON`; the conn stays usable. — `TestEdgeRouting/malformed_JSON_keeps_the_conn`
8. Unknown action → `unknown action`; second open → `already in sheet`; missing key → `key required`; `dump` → `dump moved to …`; get miss → `key not found`. — `TestEdgeRouting/unknown_action…`
9. Unlock by non-owner → `not lock owner`. — `TestEdgeRouting/unlock_by_non-owner`
10. `register_replica` on `/ws` → `register_replica moved to /replica` (duplicate register on `/replica` is S4). — `TestEdgeRouting/register_replica_on_/ws`
11. A request epoch above the server's → `stale epoch` (open and writes) and `Node.ObserveEpoch`. — `TestEdgeRouting/stale_epoch`
12. `GET /health` → exactly `{status:"ok", role, epoch, instance_id}`, `application/json`. — `TestEdgeRouting/health`
13. A frame over `REDLITE_MAX_FRAME_BYTES` closes the socket with 1009. — `TestEdgeOversizedFrame`
14. Wire compat: a v1 client (no id, version, epoch) gets `{success,sheet}`, `sheet_state{data}`, and `replicate{key,value}` / `invalidate{key}` it decodes with v1 names; a v1 set reply has no `id`. — `TestEdgeV1Client`
15. K9: 8 goroutines writing through two conns concurrently, `-race` clean, no failed set. — `TestEdgeConcurrentWritersSeq`
16. K10: every collaborator observes strictly increasing, gap-free `seq`. — `TestEdgeConcurrentWritersSeq`
17. K13: the sheet and its data survive the last collaborator leaving; the password still applies. — `TestEdgeSheetSurvivesLastLeave`
18. K14: `REDLITE_SHEET_AUTOCREATE=false` → open of an unknown sheet is `sheet not found` and creates nothing; an admin-created sheet opens. — `TestEdgeNoAutocreate`
19. K15: a conn that never reads (`REDLITE_CONN_SEND_QUEUE=4`) is kicked while a reading reader receives the next write within 2s and stays connected. — `TestEdgeSlowConsumerKicked`
20. `REDLITE_ROLE=replica` refuses `open` with `not leader`; `/health` says `replica`. — `TestEdgeReplicaRefusesOpen`
21. Shutdown mid-write: readers writing in a loop while the server stops → no panic, `Run` returns nil, writes after the stop fail with `ErrNotConnected`/`ErrTimeout`. — `TestEdgeShutdownMidWrite`
22. A panic in a connection goroutine is logged at Error and closes only that socket; `Send` is non-blocking and bounded; `SendAndClose` writes the frame before the close; an unanswered ping closes the conn. — `internal/ws` unit tests

## Surface

| Kind | Name | Package |
|---|---|---|
| type | `Node` interface `{IsLeader() bool; Epoch() uint64; ObserveEpoch(uint64)}` | internal/ws |
| type | `Conn` (`NewConn`, `ID`, `Remote`, `Send`, `SendAndClose`, `Kick`, `Done`, `Run(ctx, handle)`) | internal/ws |
| type | `Handler` (`NewHandler`, `ServeHTTP`, `Len`, `CloseAll`, `KickAll`, `Wait`) | internal/ws |
| type | `Manager` (`NewManager`, `TryAcquire`, `Release`, `ReleaseAll`, `CleanupExpired`, `Len`, `RunCleaner`), `ErrLockHeld/ErrNotOwner/ErrLockNotFound` | internal/lock |
| type | `App` (`New`, `Handle`, `Registry`, `Config`, `InstanceID`, `Conns`, `Run`) | internal/app |
| route | `GET /health`, `/ws` | internal/app |

## Value-source table

| Value | Source | Decided |
|---|---|---|
| reply `id` | `Request.ID` echoed | ✅ |
| open `sheet` | `Sheet.Name()` | ✅ |
| open `protocol_version` | `protocol.Version` | ✅ |
| open / sheet_state `epoch`, health `epoch` | `Node.Epoch()` (wave 2: 1) | ✅ D-012 |
| `seq` on replies | `sheet.Result.Seq` / Join's seq | ✅ D-014 |
| set `expires_in_ms` | `sheet.Result.ExpiresIn` (policy TTL + jitter) | ✅ D-008 |
| `deleted` | `sheet.Result.Deleted` | ✅ |
| get `value` | `Sheet.Get` entry bytes | ✅ |
| set TTL | `Request.TTL` minutes, 0 → `REDLITE_CACHE_TTL` (in `cache.Policy`) | ✅ |
| lock TTL | `Request.LockTTL` seconds, ≤0 → `REDLITE_LOCK_TTL` | ✅ |
| lock owner | `Conn.ID()` (process-unique atomic counter) | ✅ |
| health `role` | `Node.IsLeader()` | ✅ |
| health `instance_id` | `REDLITE_INSTANCE_ID` or 8 random bytes hex at `app.New` | ✅ |
| send queue, frame limit, ping, write timeout | `REDLITE_CONN_SEND_QUEUE`, `REDLITE_MAX_FRAME_BYTES`, `REDLITE_PING_INTERVAL`, `REDLITE_WRITE_TIMEOUT` | ✅ D-014 |

## Failure behavior

| Failure | What the caller sees | What the system does |
|---|---|---|
| Socket down | reader reconnects (S3) | `sheet.Leave`, `locks.ReleaseAll(conn)` |
| Sheet not open | `must open sheet first` | Warn `WS: action failed` |
| Lock held | `lock held by another client` + key | Warn `WS: action failed` |
| Slow consumer | socket closed (1001) | sheet drops it, Warn `Sheet: collaborator kicked` |
| Oversized frame | socket closed (1009) | conn ends |
| Panic in a conn | that socket closes | Error `WS: panic recovered` |
| Shutdown | socket closed (1001 "server shutting down") | listener closed, every conn kicked, wait ≤ `REDLITE_SHUTDOWN_TIMEOUT` |
| Persist unavailable | — | S5 (wave 3) |
| Replica lag | — | S4 (wave 3) |

## Reuse

`sheet.Registry.Open`, `Sheet.Join/Apply/Get/Leave`, `sheet.RunJanitor` (internal/sheet, S1);
`protocol.*` frames and `ErrMsg*`; `config.Config`; `logger.L`, `logger.Banner`; reader + harness
`RawConn`/`Eventually`/`UniqueSheet` in tests. Patterns: code-heim go_75_coder_websocket (Conn),
go_18_worker_pool (RunCleaner), go_61_err_group (App.Run) — `doc/reference/codeheim_patterns.md` § Tier 2.

## Decisions

D-002 (map + mutex), D-003 (one janitor, `invalidate{keys}`), D-010 (`/replica`), D-011 (coder/websocket),
D-012 (epoch), D-013 (autocreate), D-014 (bounded send queue, kick). Assumptions made in S2 are in the
lead's ASSUMPTIONS entries for this wave (expired-lock release = `lock not found`; stale write and get miss
logged at Debug; `set_null` honours `ttl`; default origin policy).

## Wire compatibility

Additive only; no `protocol/` change. Behaviour changes already listed in `doc/protocol/wire.md`
(`dump` moved, `register_replica` moved). Error strings are v1's except the wire.md "new" ones; v1's
`unknown action: <name>` and `already in sheet: <id>` suffixes are dropped (the frozen constants).

## Out of scope

Replica node, `/replica`, promotion, step-down (S4); persistence (S5); `/admin/*` (S6, mounted with `App.Handle`).

## Tests

| Criterion | Test file | Kind |
|---|---|---|
| 1 | testing/integration/key_test.go | integration |
| 2 | testing/integration/sweep_test.go | integration |
| 3 | testing/integration/lock_test.go, internal/lock/lock_test.go | integration + unit |
| 4–21 | testing/integration/edge_test.go | integration |
| 22 | internal/ws/conn_test.go | unit |
