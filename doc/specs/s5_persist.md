# S5 Persistence (part 1: internal/persist) — Spec

**Status:** built — writer, plan, hydration, cleaner, epoch (wave 1); app wiring
(`internal/app/wire_replica.go`), parked creates (criterion 10), one `DeleteEntries` per expiry, replica
interplay and the PERSIST loop (wave 3). The DB-backed tests have not run on this machine (no
`REDLITE_TEST_DATABASE_URL`): criteria 6–9 and 11 are written, not yet verified.
**Reference:** `advo_cache/CACHE_PERSISTENCE_REPORT.md:495-529` (Writer.worker), `:785-815`
(Loader.LoadAllEntries); code-heim go_18_worker_pool (`doc/reference/codeheim_patterns.md` § Tier 2);
design `doc/persist/design.md`; rules `.claude/rules/backend/go-persist.md`. Serves K13 (sheets outlive
connections), D-012 (epoch storage), D-013, D-015.

## Goal

Keep a durable PostgreSQL copy of every sheet and entry the leader applies, without ever blocking a
sheet on the database, and load it back into memory at boot so a restart of the backendAdvo cache keeps
its sheets, passwords and live entries.

## Acceptance criteria

1. `Writer.Observe` never blocks: with the queue full it returns immediately, the op is dropped,
   `Dropped()` counts it, and `Persist: enqueue dropped` is logged at Warn at most once per second with
   `queue_len`, `cap` and the `dropped` count since the last line.
2. A writer whose `active()` is false (replica) queues nothing and drops nothing.
3. `Run` flushes when the batch reaches `BatchSize`, when `FlushInterval` elapses with a non-empty batch,
   and on ctx cancel (draining what is already queued, with its own 10s deadline), then returns nil.
4. A batch collapses per (project, sheet, key) to the last op: set,set → last set; set,delete → delete;
   delete,set → set; a prefix/suffix delete cancels earlier sets/deletes it covers in the same sheet and
   a later set of a covered key survives after it; expire is one multi-key delete; a sheet create is never
   collapsed or moved. The final table state equals running every op in order.
5. A batch is one transaction. A failed flush logs `Persist: flush failed` at Error and is retried once,
   before the newer batch, at the next flush; a second failure logs `Persist: batch dropped` with `ops`.
   A failure in the shutdown flush is dropped with the same Error.
6. `Hydrate` returns every sheet (project, name, hash) and its live entries via `load`, with raw JSON values
   and absolute `ExpiresAt` (zero = no expiry); expired rows are never loaded.
7. `RunCleaner` deletes expired rows every interval (≤10000 a pass), logs Debug `rows_deleted`, logs Error
   on a failed pass and keeps running, and returns nil on ctx.
8. `LoadEpoch` returns 0 with no row; `RaiseEpoch` is monotonic (a lower epoch returns the stored one).
9. PERSIST loop (wave 3, end to end against a running leader): set → flush → restart → hydrated; expired
   rows not loaded; the password still required; full queue drops with a Warn, never blocks.
10. A sheet create is never dropped: on a full queue (or `EnsureSheet`) it is parked and upserted at the
    head of the next flush; a dropped batch parks its creates again. A node promoted to leader ensures
    every sheet it holds. (Fixes the FK poisoning found in wave 1: one lost create failed every later
    batch touching that sheet.) An expiry pass is one `DeleteEntries` (keys `text[]`) statement.
11. Wiring: with `REDLITE_PERSIST_ENABLED`, boot migrates, opens the pool, and a leader hydrates before the
    listener exists (Info `Persist: hydrated` sheets, entries; Error `Persist: hydration failed` fails
    start-up); the writer persists only while the node is leader; the epoch survives restart and promotion;
    the pool closes after the writer, cleaner and node have returned.
12. **Fenced writes refused.** Every flush transaction reads `cluster_state.epoch` first; stored > the
    node's epoch (`Options.Epoch`) → the batch is dropped, never retried, Error
    `Persist: flush refused, stale epoch` (`Writer.Refused()` counts). Plain read, no FOR UPDATE.
13. **Shutdown ordering.** The writer and the cleaner run on their own ctx, cancelled only after the HTTP
    server shut down and every `/ws` connection closed (`App.connsClosed`, backstop ShutdownTimeout); the
    writer then drains its whole queue and flushes it with its own 10s deadline.
14. **D-020.** At boot, after Migrate, `cache_entries` is set LOGGED when `REDLITE_PERSIST_LOGGED=true`
    and UNLOGGED when false (pg_class read first; ALTER only on a change); Info
    `Persist: cache_entries durability` (logged, changed).
15. **D-021 control bus.** `persist.Notify` publishes `{"kind":"project_revoked","project":"<uuid>"}` on
    `redlite_control`; with `REDLITE_CONTROL_LISTEN` every instance runs one `RunControlListener` on a
    dedicated connection (reconnect with backoff) that drops the project's sheets from memory and kicks
    their collaborators. Fallback: each cleaner tick `Reconciler.Pass` drops every in-memory sheet that
    Postgres had at the previous pass and no longer has (a never-flushed new sheet is never dropped).
    Every path that drops sheets (route, listener, reconcile) also tells the writer
    (`RevokeProject` / `RevokeSheets`): it discards every queued, held and parked op of those sheets
    accepted before the call, waits out a flush in flight, and prunes the revocation once queue and
    held batch are empty, so a revoked sheet never comes back in Postgres.

## Surface

| Kind | Name | Package |
|---|---|---|
| type | `Options{BatchSize, FlushInterval, QueueSize, CleanupInterval}` | internal/persist |
| func | `NewWriter(store *db.Store, o Options, active func() bool) *Writer` (nil active = always) | internal/persist |
| method | `(*Writer).Observe(sheet.Op)` — `sheet.Observer` | internal/persist |
| method | `(*Writer).Run(ctx) error` | internal/persist |
| method | `(*Writer).Dropped() uint64` (addition: drop counter for tests/health) | internal/persist |
| type | `SheetRow`, `EntryRow` | internal/persist |
| func | `Hydrate(ctx, store, load) (sheets, entries int, err error)` | internal/persist |
| func | `RunCleaner(ctx, store, interval) error` | internal/persist |
| func | `LoadEpoch(ctx, store) (uint64, error)`, `RaiseEpoch(ctx, store, epoch, instanceID) (uint64, error)` | internal/persist |

## Value-source table

| Value | Source (field / config / computed from) | Decided |
|---|---|---|
| batch size | `REDLITE_PERSIST_BATCH_SIZE` (100) → `Options.BatchSize` | ✅ design.md §3 |
| flush interval | `REDLITE_PERSIST_FLUSH_INTERVAL` (5s) → `Options.FlushInterval` | ✅ design.md §3 |
| queue size | `REDLITE_PERSIST_QUEUE_SIZE` (10000) → `Options.QueueSize` | ✅ internal/config |
| cleanup interval | `REDLITE_PERSIST_CLEANUP_INTERVAL` (60s) | ✅ design.md env |
| shutdown flush deadline | 10s constant (also bounds every flush) | ✅ design.md Flows |
| sheets.project_id / name / password_hash | `sheet.Op` OpCreate `ProjectID`, `Sheet`, `PasswordHash` | ✅ op.go |
| sheets.autocreated | `Op.ProjectID == DefaultProject` (all-zero uuid) | ✅ brief, D-013 |
| cache_entries.value | `Op.Value` (raw JSON, never decoded) | ✅ op.go |
| cache_entries.expires_at | `Op.ExpiresAt`; zero → NULL | ✅ entry.go, D-003 |
| prefix / suffix match | `Op.Prefix` / `Op.Suffix` via `starts_with` / `right()` | ✅ entries.sql |
| expired keys | `Op.Keys` of OpExpire | ✅ op.go |
| hydrated ExpiresAt | `cache_entries.expires_at` (NULL → zero) | ✅ |
| epoch | `cluster_state.epoch` (bigint; > MaxInt64 refused) | ✅ D-012 |

## Failure behavior

| Failure | What the caller sees | What the system does |
|---|---|---|
| Queue full | nothing; Observe returns | op dropped, `Dropped()`++, Warn ≤1/s |
| Persist unavailable | nothing on the WS path | flush fails → Error, retried once, then dropped (Error, `ops`) |
| Shutdown with DB down | `Run` returns nil after ≤10s | held + current batch dropped with Error |
| Hydration fails | `Hydrate` returns a wrapped error | caller logs `Persist: hydration failed` and decides |
| Cleaner pass fails | nothing | Error, next pass runs on schedule |
| Replica | — | `active()` false: Observe ignores ops |

## Reuse

`db.Store.ExecTx` + the generated `registry`/`entries`/`cluster` queriers (no hand-written SQL);
`sheet.Op`/`sheet.Observer` (op.go); `cache.Entry` (entry.go); `logger.L`. Worker shape after code-heim
go_18_worker_pool; the advo_cache report's Writer/Loader for the flow.

## Decisions

D-003 (zero expiry), D-012 (epoch), D-013 (registry-owned sheets), D-015 (pgx + sqlc + migrate).
Assumptions for the lead to log: flush success is Debug, not Info (per-5s noise); a size-triggered flush
also counts as "the next tick" for the one retry; the final shutdown flush has no retry; `ExecTx` (one
round trip per statement) instead of `Store.Batch`, because the SQL lives only in the generated package.

## Wire compatibility

None: no protocol change.

## Out of scope

Replica-shares-database behaviour beyond `active()` and `EnsureSheet`, quotas, export/import (S6).

## Tests

| Criterion | Test file | Kind |
|---|---|---|
| 1 | `internal/persist/writer_test.go` TestObserveNeverBlocksWhenFull | unit |
| 2 | `internal/persist/writer_test.go` TestInactiveWriterIgnoresOps | unit |
| 3 | `internal/persist/writer_test.go` TestRunFlushesOnSize / OnInterval / OnCancel | unit |
| 4 | `internal/persist/writer_test.go` TestPlanCollapse, TestPlanMapsCreateAndExpiry | unit |
| 5 | `internal/persist/writer_test.go` TestRunRetriesOnceThenDrops | unit |
| 6, 7 | `internal/persist/db_test.go` TestDBWriterHydrateRoundTrip | DB (skips without `REDLITE_TEST_DATABASE_URL`) |
| 8 | `internal/persist/db_test.go` TestDBEpochMonotonic | DB (skips) |
| 9 | `testing/integration/persist_test.go` TestPersistLoop, TestPersistQueueFullNeverBlocks | integration (DB, skips) |
| 10 | `internal/persist/writer_test.go` TestCreateParkedOnFullQueue, TestDroppedBatchReparksCreate, TestEnsureSheetFlushesCreate, TestPlanCollapse (expire cases) | unit |
| 11 | `testing/integration/persist_test.go` TestPersistLoop, TestPersistEpochAcrossPromotion | integration (DB, skips) |
| 12 | `internal/persist/writer_test.go` TestStaleEpochFlushRefusedNotRetried, TestCheckEpoch; `db_test.go` TestDBFlushRefusedForStaleEpoch | unit; DB (skips) |
| 13 | `internal/persist/writer_test.go` TestRunDrainsWholeQueueOnCancel; `persist_test.go` TestPersistShutdownFlushesEverything | unit; integration (DB, skips) |
| 14 | `testing/integration/persist_test.go` TestPersistLoggedFlag | integration (DB, skips) |
| 15 | `testing/integration/control_test.go` TestRevokeEveryInstanceByNotify, TestRevokeEveryInstanceByReconcile (DB, skips); `writer_test.go` TestRevokeDiscardsQueuedAndParkedOps, TestRevokeDiscardsHeldBatch | integration; unit |
