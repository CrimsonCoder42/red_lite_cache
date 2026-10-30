# Leaks 2026-10-29 — work folder (THE BRIEF + progress)

Request: prove red_lite_cache holds no lingering data (D-038…D-042). Agents read this instead of
re-orienting. Heap profiles/traces only under `doc/leaks_2026-10-29/out/` (gitignored; moved to `_delete/`
once their numbers are here).

## Baseline

`bash testing/run.sh` before any change: `Total | Passed | Failed | Skipped : 327 | 314 | 0 | 13`
(13 = persist, `SKIPPED: persist (no REDLITE_TEST_DATABASE_URL)`).

## Built by the lead (wave 0) — landed, verified `go build ./...`, `bash testing/run.sh internal/cache internal/sheet internal/config internal/lock` → `81 | 81 | 0 | 0`

- `internal/cache/store.go` `Stats()` → `cache.Stats{Entries, RootBuckets, TailBuckets, HeadBuckets, HeadRoots}`.
  The store has no clock of its own: every time-dependent call takes `now` (`Get/Has/Expire/All(now)`), so the
  caller's clock is the seam.
- `internal/sheet/registry.go` `Registry.Stats()` → `sheet.Stats{Sheets, Collaborators, SweepLogEntries, Store cache.Stats}`
  (summed per sheet, each under its own mutex); `sheet.go` `(*Sheet).stats()`. The registry clock already
  existed: `sheet.Options.Now` (`registry.go:39`, default `time.Now`).
- `internal/config/config.go` one marked section "leak bounds + test clock": `TenantMemoryMax`
  (`REDLITE_TENANT_MEMORY_MAX`, default 10 000, must be ≥ 1) and `Clock func() time.Time` (never from env).
- `internal/app/app.go` passes `cfg.Clock` to `sheet.Options.Now` and `lock.NewManager`; `App.Locks()` accessor.
  `tenant.New` is NOT yet given `TenantMemoryMax` — the lead wires that in wave 2 (see Agent B).
- `testing/integration/harness_leaks.go`: `NewClock()`, `(*Clock).Now/Advance`, `WithClock(c)` (a StartLeader /
  StartReplica mod), `Goroutines(t)` (settle: 20 × 10 ms, two equal samples), `GoroutineBaseline(t)` (two settled
  passes), `WaitGoroutines(t, want, tol)` (bounded by `Timeout`), `HeapAlloc()` (2 × `runtime.GC` then `HeapAlloc`).
- `testing/leaks/leaks.go` (package `leaks`): `NewSteps(t)` with `.Baseline/.Fill/.Observe/.Drain/.Assert` (each
  logs the step + elapsed), `.Elapsed()`, `Above(t, what, peak, base)` (fails when peak ≤ base), `Within(t, what, got, want, tol)`.

Janitor and lock cleaner still tick on REAL time (TestConfig: 50 ms) — with `WithClock`, what they compare
against is the test clock. A test advances the clock, then either calls the pass directly
(`srv.App.Registry().ExpireAll(clock.Now())`, `srv.App.Locks().CleanupExpired(clock.Now())`) or waits with
`integration.Eventually` (a condition with a deadline, never a sleep past a TTL).

## Per-place table — verified by the lead (file:line at wave 0)

| # | Place | Bounded? | Freed on | Existing test (read) | Leak test |
|---|---|---|---|---|---|
| 1 | `cache.Store` map + root/tail/head buckets | by `CacheMaxKeysPerSheet` (default 0 = **unbounded**, `config.go` `REDLITE_CACHE_MAX_KEYS_PER_SHEET`) and TTL | `remove()` drops each emptied bucket (`store.go:131-165`); `DeletePrefix/Suffix("")` → `clear()`→`reset()` (`:295-299`); `Expire(now)` (`:303-316`); `Replace` (`:341`) | `TestHeadIndexLifecycle`, `TestIndexRandomOps`, `TestLazyExpiry` (internal/cache) | L1, L12 |
| 2 | janitor | one goroutine, ctx exit (`sheet/janitor.go:17-33`), skips on replica | — | `TestRunJanitor` | L2 |
| 3 | expiry broadcast | one `invalidate{keys}` per sheet per pass (`sheet.go:337-355`) | reader deletes the keys (`reader/local.go:279-295`) | `TestKeyLoop` | L2 |
| 4 | sweep log | `SweepLogMax` 4096 + `SweepLogWindow` 30 s (`config.go` build) | pruned only on `add`/`stale` (`sweeplog.go:45-67`): an idle sheet keeps ≤ max entries past the window until its next write; `entries[drop:]` keeps the backing array until append reallocates (≤ ~2× max) | sheet R6 tests | L5 |
| 5 | sheet collaborators | per conn | `Leave` (`sheet.go:162`), full queue (`:176-184`), `kickAll`/`drop` (`:358-380`); conn end → `router.go:258-265` (Leave, endSession, ReleaseAll) | `TestBackpressureKicks` | L6, L7 |
| 6 | reader local map | `MaxEntries` (default 100 000, `reader.go:44`) | `evictLocked` (`local.go:213-232`; evictScan 64 `:15`), invalidate, replace | `TestR4ExpiryAndBound` | L3 |
| 7 | reader miss records | `missSeqMax` 65 536 (`reader.go:53`) | record with no live entry dropped only on overflow (`local.go:146-152`) or by `baseSeq` of that key (`:181-186`) | R6 reader tests | L3 |
| 8 | reader pending deletes | `MaxPendingDeletes` 10 000 (`reader.go:46,108`), overflow collapses to one `delete_prefix ""` (D-005) | replay on reconnect | `TestR2QueueOverflowCollapses` | L4 |
| 9 | reader per-session `pending` calls | in-flight writes | teardown (`reader/conn.go:625-629`) | `TestCloseIdempotentNoLeak` | L2/L12 (goroutines) |
| 10 | ws `Conn` send queue | `ConnSendQueue` 256 (`ws/conn.go:23,70-84`) | unreachable after close; goroutines: read (the http handler's), writer, ping (`conn.go` Run) | `TestConnQueueBoundAndKick` | L6 |
| 11 | ws handler `conns` | per conn | `untrack` (`ws/handler.go:105-122`) | — | L6 |
| 12 | session registry | per conn | `Remove` (`session/session.go:124`) | session tests | L6, L7 |
| 13 | tenant `suspend` | by #projects | `SetSuspended(false)`, `ReplaceSuspended` (`tenant.go:105-132`) | suspend tests | L7 |
| 14 | tenant `revoked` names / `deadKey` ids | **UNBOUNDED** (`tenant.go:50-51`); grow at `api/revoke.go:129`, `app/wire_replica.go:191` (names), `api/tenancy.go:280`, `wire_replica.go:322` (keys) | never | — | L8 (D-041) |
| 15 | locks + owner index | per conn × keys | `ReleaseAll` on conn end (`router.go:262`), `CleanupExpired` tick, `dropLocked` drops an empty owner bucket (`lock.go:162-170`) | `TestExpiredTakeoverAndRelease`, `TestReleaseAllAndCleanup` | L9 |
| 16 | persist queue | `PersistQueueSize` 10 000 (chan cap, `writer.go:212`) | flush; full → drop, Warn ≤ 1/s with the count (`writer.go:224-253`, `dropWarnEvery`) | `TestObserveNeverBlocksWhenFull` | L10 |
| 17 | persist parked creates | by #sheets (map by name, `writer.go:90-96`) | `takeParked` on flush (`:276-290`) | writer tests | L10 |
| 18 | background goroutines | janitor, lock cleaner, replica node, persist writer/cleaner/toucher/audit/control — all in one errgroup (`app.go:172-215`, `wire_replica.go:205-235`) | ctx | `cmd/redlite` tests | L11, L12 |

## Owned paths

| Agent | Owns (edit ONLY these) | Delivers |
|---|---|---|
| A — server store + sheet + lock | `internal/cache/**`, `internal/sheet/**`, `internal/lock/**` (do not change the Stats types the lead added; extend only if a test needs a field), `testing/leaks/store_test.go` | `lock.Manager.Stats() lock.Stats{Held, Owners}`; L1, L2 (server half: leader + 3 raw/reader conns, server Entries 0), L5, L9; edge: Expire on empty store, DeletePrefix("") on non-colon keys, janitor pass with zero sheets |
| B — ws + session + tenant + persist | `internal/ws/**`, `internal/session/**`, `internal/tenant/**`, `internal/persist/**`, `testing/leaks/conn_test.go` | `ws.Conn.Queued()` (+ handler count if needed), `tenant.Directory.Stats() tenant.Stats{Suspended, RevokedNames, DeadKeys}`, D-041 bound in tenant (cap via a setter or option with default 10 000 when unset, oldest dropped, one Warn `Tenant`-module… use module `API` or add none — see Logging below), `(*Directory).ResetRevocations()` for the Postgres reload, `persist.Writer.Stats() persist.Stats{Queued, Parked, Held, Dropped}`; L6, L7, L8, L10; edge: kick of a session mid-open, suspend of a project with zero sessions |
| C — reader | `reader/*.go` (NOT `reader/v1compat/**`), `testing/leaks/reader_test.go` | `reader.Stats` type + `(*Reader).Stats()` {Entries, PendingDeletes, MissRecords} (doc: diagnostic, not part of the compatibility contract); clock seam inside the reader (unexported field, default time.Now — see below); L3, L4, L2 (reader half: 3 readers' Entries 0 within one pass); edge: a reader closed while a replicate_expire batch is in flight |
| Lead | `internal/config/config.go` (marked section), `internal/app/**`, `protocol/messages.go` (one `Stats` field on Health + its type, wave 2), `testing/integration/harness_leaks.go`, `testing/leaks/leaks.go`, `testing/leaks/e2e_test.go`, docs/ledgers | L11, L12, `/health` stats (dev only), wiring of B's tenant cap + reset into app |

**Shared-file order:** nobody but the lead edits `config.go`, `app/**`, `protocol/**`, `harness*.go`, ledgers. An
agent that needs a harness or app change writes it in its report; the lead makes it. Each agent edits only its
own `testing/leaks/<layer>_test.go`.

## Rules for the leak tests (D-038)

- Line 1 of every file: `// Covers: memory and goroutine bounds`. Package `leaks`. No `t.Parallel()` — goroutine
  counts are process-wide.
- Five steps via `NewSteps(t)`; `Above(...)` on every peak before asserting the return. Tolerances: stated in a
  comment with the reason; never widened to make a test pass (a red twice is a LEAK → fix it under BUG).
- Goroutines: `GoroutineBaseline(t)` after the harness is up; after DRAIN `WaitGoroutines(t, base, tol)`.
  Memory: `HeapAlloc()` only (never `Sys`).
- Clock: `c := integration.NewClock(); srv := integration.StartLeader(t, integration.WithClock(c))`; advance, never
  sleep past a TTL. The REDLITE TTL unit on the wire is minutes; `cfg.CacheTTL` is the default for ttl 0.
- Numbers are the request's (L1–L12 in `.claude/prompts`-style request; restated per agent prompt). Record peak /
  baseline / final and wall time in your report; the lead copies them below.
- Logging: a new bound's first-drop Warn follows `.claude/rules/backend/go-logging.md` (`Module: event`, ids/counts only).
  Module for tenant: `API` (the tenancy module list has no `Tenant`; the existing tenant code logs nothing) — or say
  in the report if another fits better.
- Run: `bash testing/run.sh testing/leaks` and `bash testing/run.sh internal/<your pkg>`; `-race` is the harness default.
- Reader clock (D-039): the reader is public; a clock exported through `reader.Config` would be a new public
  field. Do it as an unexported field only (in-package tests may set it); L2–L4 need no reader clock (expiry
  reaches readers as `invalidate{keys}`; L3/L4 use no TTL). State this in the report.

## Progress — results from real runs (`go test -race`, the leak suite run with `-count=2`, L12 with `-count=3`)

| Test | Owner | Result | Baseline → peak → final | Wall |
|---|---|---|---|---|
| L1 StoreExpireFreesEntriesAndIndex | A | PASS | 0 → {Entries 10 000, Root 8, Tail 510, Head 400, HeadRoots 8} → 0 after Expire, after 400 head DeletePrefix, after Replace(∅); heap 1.83 → 4.38 → 0.33 MB | 0.22 s |
| L2 JanitorBroadcastsAndFrees (server) | A | PASS | 0 → 5 000 entries, 200 heads → 0, no reader Has any key | 0.89 s |
| L2 ReaderEntriesFreedByExpire (readers) | C | PASS | 3 readers 0 → 5 000 each → 0 after one pass; goroutines 25 → 25 | 0.70 s |
| L3 ReaderBoundHoldsUnderChurn | C | PASS | Entries ≤ 1 000 at all 50 samples; MissRecords 0; heap @10 2 249 776 → @50 2 286 384 B (+1.6%) | 5.5 s |
| L4 PendingDeletesBound | C | PASS | 0 → 10 000 (cap) → 1 (collapsed `delete_prefix ""`) → 0 after reconnect | 0.17 s |
| L5 SweepLogBound | A | PASS | 0 → ≤ 4 096 over 50 000 writes → 1 after clock + window + one write → 0 after an idle janitor pass | 0.57 s |
| L6 ConnCloseFreesQueueAndGoroutines | B | PASS | goroutines 13 → 613 (3 per conn: read + writer + ping; the client adds 0) → 13; 36 800 frames queued; sessions/conns 201 → 0; locks 200 → 0 | 3.6 s |
| L7 SessionKickAndSuspendFree | B | PASS | goroutines 10 → 210 → 50 (10 survivors × 4) → 10; sessions 50 → 10; Suspended 2; RevokedNames 2 | 0.84 s |
| L8 TenantSetsBounded | B | PASS | DeadKeys / RevokedNames 12 000 marked → 10 000 held, 2 000 dropped each, 1 Warn per set; ResetRevocations → 0 | 0.05 s |
| L9 LockCleanupFrees | A | PASS | {Held 5 000, Owners 100}, goroutines 10 → 310 → {2 500, 50} after 50 closes → {0, 0} after clock + 2 s; goroutines 10 | 1.9 s |
| L10 PersistQueueBound | B | PASS (bound half); Postgres half `SKIPPED: persist` | Queued 1 000, Dropped 4 000, 1 Warn, Parked 10 → 0/0/0 | 0.07 s |
| L11 ReplicaStepDownFrees | lead | PASS | 2 000 keys; new leader {Sheets 1, Collab 3, Entries 2 000}; rejoined old leader {1, 0, 2 000}; stopped leader Collab 0 | 1.1 s |
| L12 EndToEndBaseline | lead | PASS 12/12 (`-count=12`) after the checker's flake fix | goroutines 25 → 40 → 25; heap 0.78 → 26.8 MB → **+0.5 … +1.5 %**, first run in a process **+4.7 %** (bound 5 %, thin margin) once the F-1 residue is dropped; leader Sheets 8, every other count 0. Checker before the fix: 2/3 at `-count=3`, one run +5.3 % — single heap samples + an idle admin keep-alive conn; now min of 3 settled samples at both ends and `req.Close` (bound unchanged) | 1.6–2.0 s |
| TestHealthStatsDevelopmentOnly | lead | PASS | stats present in development (to a token holder), absent in staging | — |
| Edge cases (6) | A/B/C | PASS | Expire on ∅, DeletePrefix("") on non-colon keys, janitor with 0 sheets / inactive, reader closed mid-expire batch, kick mid-open, suspend with 0 sessions | — |

## Leaks found and fixed

| # | Place | Missing free | Fix | Proof |
|---|---|---|---|---|
| 1 | sweep log, idle sheet | pruned only on write/stale; kept ≤ 4 096 entries + backing array for life | janitor pass prunes by age every pass (`sheet.go` expire); an emptied log is `nil` (`sweeplog.go`) | L5, `TestJanitorPrunesSweepLog` |
| 2 | sweep log, replica | janitor skipped the whole pass on a replica | inactive janitor calls `Registry.PruneSweepLogs` (`janitor.go`, `registry.go`) | `TestEdgeStoreInactiveJanitorPrunes` |
| 3 | reader miss records | a key missed once and never set kept its record until 65 536 accumulated | once per MissWindow the first new-key miss drops every record with no live entry (`reader/local.go` sweepMissLocked) | `TestMissRecordsSweptAfterWindow` (1 001 → 2) |
| 4 | closed reader | map, queue and miss records held as long as the consumer held `*Reader` | `release()` after the last loop exits (Close, ctx cancel, 4001/4002/4003) | `TestStatsFreedOnClose`, `…TerminalClose` |
| 5 | tenant revoked names / dead keys | unbounded | `fifoSet` cap `REDLITE_TENANT_MEMORY_MAX` (10 000), oldest dropped, one Warn (`internal/tenant/bounded.go`) | L8 |
| 6 | server store + reader map memory | a Go map never shrinks on delete: a drained store kept its peak bucket arrays (L12 heap +1004 % before) | the maps are replaced when the last key goes (`cache/store.go` remove, `reader/local.go` shrinkLocked) | L12 heap +1004 % → +375 % (rest = F-1) → +1.6–3.9 % |

## Found, not fixed

- **F-1 (recommended D-043):** with persistence off, a project revoke reaches the replica only as `key_revoked`;
  dropping the sheets travels on the Postgres NOTIFY bus alone (`app/wire_replica.go` applyControl), so the
  replica keeps the revoked project's sheets and keys until its next resync — and would serve them after a
  promotion. L12 measured 5 000 keys, 2 sheets, ~2.9 MB (heap profile at ASSERT: 86 % under
  `replica.(*Node).follow`). Options: a `project_revoked` replica control frame (additive wire type — the
  request allowed no wire change but `/health` stats), or kicking the replica stream after a revoke so it
  resyncs (no wire change, full snapshot cost). Needs Devin.
- **D-041 reset not wired:** `tenant.ResetRevocations` exists and is tested (L8) but is not called from the
  Postgres reconcile: Postgres keeps no record of revoked sheet names (the rows are deleted), so a reset would
  let development autocreate recreate a revoked name (D-025). Keys are safe to reset (the store no longer
  resolves them) except an open racing the reset. Both sets are bounded now, so nothing grows.
- A killed connection whose peer never reads holds its session and 3 goroutines ~5 s (coder/websocket close
  handshake wait) — bounded, seen as the 5 s wall of `TestEdgeConnKickMidOpen`.
- First-run heap: one-time process caches (encoding/json type cache, http transport) measured +7.2 % on the
  first L12 run in a process; L12 now warms every path before BASELINE (`e2eWarm`).

Heap profiles (`out/l12.mem`, `out/l12_assert.heap`, `out/leaks.test`) moved to `_delete/` once these numbers were recorded.
