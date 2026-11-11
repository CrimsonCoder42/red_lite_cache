# Reader local-map optimisation — report (2026-11-10, D-043)

Machine: darwin/arm64, 10 CPUs, Go 1.27.3. Benchmarks are medians of count=6. benchstat is not installed.
The machine's speed drifted during the day: HEAD re-run at the end was 10–30% slower than the S0 run of
the same code, so every "after" number is compared with HEAD **run back to back** (`bench_head_rerun.txt`,
a pristine `git archive HEAD` copy plus `reader/audit_test.go`), and the S0 column is shown as recorded.

## S0 — baseline

- Harness `bash testing/run.sh`: `358 | 344 | 0 | 14` (14 = persist, no `REDLITE_TEST_DATABASE_URL`), 0 data races —
  `testing/runs/20261110_101751__.log`.
- Benchmarks in `reader/audit_test.go`: keys shaped like backendAdvo's — 4 in 5 `<root>:patient:<uuid>:<view>`
  (8 roots × 5 views per profile), 1 in 5 `<entity>:record:<uuid>`. `BenchmarkLocalDeletePrefix` (one-match prefix =
  a whole patient key), `BenchmarkLocalDeleteSuffix` (one-match `:record:<uuid>`), `BenchmarkLocalGetParallel`
  (clock read + lookup on every CPU), `BenchmarkLocalPutFull` (new key on a map at MaxEntries),
  `BenchmarkLocalBytesPerEntry` (heap per entry, keys and values excluded). `out/bench_before.txt`.

## S1 — panics are visible

| Where | What |
|---|---|
| `internal/ws/conn.go:27-30`, `:205-207` | `ConnOptions.Panics *atomic.Uint64`; `guard` adds 1 per recovered panic |
| `internal/ws/handler.go:59`, `:94`, `:128` | the Handler owns the counter, passes it to every client Conn, `Handler.Panics()` |
| `internal/app/app.go:187`, `protocol/messages.go:258` | `App.Stats().Panics` → `/health` `stats.panics` (development only, D-040) |
| `reader/reader.go:144`, `:427-438`; `reader/conn.go:539` | `Reader.Stats().Panics`, survives Close |
| `testing/integration/harness_server.go:158-165`, `:197-209` | every `StartOn` server and every `NewReaderAt` reader fails its test at cleanup when its count > 0; `CheckNoPanics` for readers built directly (`testing/stress` churn reader) |

Tests (written red first: build failed on the missing fields): `TestPanicCounted` (reader, panics on purpose via a
session with no socket), `TestConnPanicCounted` (internal/ws, panics on purpose). Harness check covers
`testing/{integration,conformance,leaks,stress}`; S1 packages run: `224 | 213 | 0 | 11`, no panic counted.

## S2 — reader index

**Shared, not copied.** The index moved unchanged from `internal/cache/store.go` into `internal/cache/index.go`
(`keyIndex` `:54`, `addKey` `:73`, `dropKey` `:101`, `prefixKeys` `:146`, `suffixKeys` `:184`); `Store` embeds it
(`store.go:22-26`), so its fields and `store_test.go`'s checks of them (`s.roots`, `headOf`) are unchanged — that file
was not edited. The reader uses the exported wrapper `cache.Index` (`index.go:222-247`). `internal/cache`,
`internal/sheet`, conformance after the move: `136 | 136 | 0 | 0`. Server store benches, HEAD vs after, back to back
(`out/bench_store_head_rerun.txt`, `bench_store_after.txt`): indexed sweep 685 892 → 679 620 ns, miss sweep 49.5 → 51.7,
Get 179.1 → 180.4, Set 60.6 → 56.0 — no change beyond noise.

Reader (`reader/local.go`): `putLocked` `:288` adds a new key to the index, `deleteLocked` `:311` removes it,
`clearLocked` `:332` resets both, `removeLocked` `:386` asks the index for a prefix/suffix's matches (reused victims
buffer, dropped above `maxVictimsBuf` 1 024, `:381`), `replace` `:459` builds the map and index before the lock.
`reader.Stats` gains `RootBuckets/TailBuckets/HeadBuckets` (`reader.go:424-426`). Consequence: `reader/` now imports
`internal/cache` (stdlib-only); spec criterion 11 updated.

| Criterion | Result | Evidence |
|---|---|---|
| (a) randomized vs naive scan | **PASS** | `TestLocalIndexMatchesScan`: 120 000 ops (put, delete, prefix, suffix, invalidate frames, rare clears) over 3 000 keys of every index shape; sizes equal after every op, key sets and bucket counts equal every 1 000 ops (`reader/index_test.go`) |
| (b) ≤ 3× from 1 000 to 100 000 | **PASS** | prefix 731 → 954 ns (1.30×), suffix 664 → 873 ns (1.31×) (`out/bench_after.txt`); was 12.6 µs → 1.50 ms (119×) |
| (c) Get, put ≤ 10% slower | **Get PASS, put FAIL at 100 000** | table below |
| (d) leaks green, buckets → 0 | **PASS** | `TestL13ReaderIndexDrains` (real leader, 6 000 keys, 1 200 head buckets → 0/0/0 after exact, head-prefix, root-prefix and suffix deletes); `TestLocalIndexDrainsToZero` (every removal path incl. eviction and sheet_state) |
| (e) bytes per entry | 94.4 → 338.9 B | `BenchmarkLocalBytesPerEntry` (S2 alone: 291.5; S3's slot array +47) |

Gain at 10 000 entries (the ⏸ threshold was 2×): prefix 168 316 → 799 ns, 211×.

## S3 — sampled LRU (D-043)

Red first: `TestEvictionKeepsHotKeys` kept **0 of 100** on the arbitrary eviction. Final design (`reader/local.go`):
`slot{key, access, expires}` `:45`, one per key in `local.slots` with swap-remove (`deleteLocked` `:311`); `evictLocked`
`:351` draws `evictSample` = 5 (`:18`) uniform slots through `draw` (`:136`, `rand.IntN`; a fixed sequence in tests),
evicts the first expired one, else the least recently read; `lookup` `:185` stores the access time atomically under the
**read** lock, at most once per `accessResolution` = 100 ms (`:27`); `mu` padded to its own cache line.

How it got there, each step measured:

| Variant | Hot keys kept (of 100) | Parallel Get vs HEAD | Put full 100k vs HEAD |
|---|---|---|---|
| arbitrary eviction (HEAD) | 0 | — | — |
| one 5-entry map-range window | 90 | — | — |
| five 1-entry map ranges | 96–99 (3 runs) | — | — |
| uniform draws, `*localEntry` with atomic access (ms) | 100 (5 runs) | −7 / −6 / −4 % | +84% |
| uniform draws, slot array, access per ms | 100 | +20 / +17 / +21 % | +43% |
| + 1 s resolution | 100 | +26 / +20 / +17 % | — |
| + mu on its own cache line | 100 at a 1 s step; **0 at a 1 ms step** (checker) | +1.0 / +4.8 / +2.7 % | +43% |
| resolution 1 ms (interleaved) | 100 at 1 s / 10 ms / 1 ms | −1 / +2 / **+22 %** | — |
| **resolution 100 ms (final, interleaved)** | **100 at 1 s / 10 ms / 1 ms** | **−5 / −4 / +0.5 %** | **+42%** |

Resolution trade-off: LRU holds while the map takes longer than the resolution to turn over (keys stored since a hot
key's last recorded read otherwise look newer than it); a finer resolution makes nearly every Get at 100 000 entries
store. 100 ms holds LRU up to 10 map turnovers a second (a 1 000-entry map at 10 000 inserts/s; at the default
100 000 cap, 1 000 000 inserts/s).

Why the map range failed: a range starts at a random slot but returns the next full one, favouring entries next to
holes — the hot keys whose neighbours were evicted (probe `_delete/reader/probe_evict_2026-11-10_test.go`: windows of 5
held 3+ hot keys 3.1% of the time against 0.86% expected, all 5 hot 6 times in 10 000 against 0.1). Why the padding:
with the touch disabled Get was level with HEAD (144 / 147 / 161 ns); with a load-only touch it was 176–188 ns;
with `mu` padded it was level — the slot header shared `mu`'s cache line, which every RLock/RUnlock writes.

`TestEvictionSample` pins the policy with a fixed draw (expired in the sample beats an older live one; else the oldest
of the sample; entries not drawn untouched; slot positions consistent). `TestEvictionPrefersExpired`, written first,
asserted "any expired entry anywhere is evicted first" — true of the 64-entry scan, not of a 5-entry sample (with 1
expired in 5 the sample misses it 33% of the time): it failed 1 run in 5 and was replaced, not retried.

## Benchmarks (ns/op, medians of 6)

| | S0 before | HEAD back to back | after | after vs HEAD |
|---|---|---|---|---|
| DeletePrefix 1 000 / 10 000 / 100 000 | 12 584 / 128 710 / 1 496 944 | 16 210 / 168 316 / 1 865 210 | 731 / 799 / 954 | ×0.045 / ×0.0047 / ×0.00051 |
| DeleteSuffix 1 000 / 10 000 / 100 000 | 12 936 / 128 499 / 1 598 889 | 16 498 / 164 718 / 2 048 920 | 664 / 657 / 873 | ×0.040 / ×0.0040 / ×0.00043 |
| GetParallel 1 000 / 10 000 / 100 000 (final, interleaved 3 × 2) | 132.5 / 131.6 / 144.9 | 154.5 / 156.7 / 147.3 | 147.1 / 150.0 / 148.0 | −5 / −4 / +0.5 % |
| PutFull 1 000 / 10 000 / 100 000 (final, interleaved 3 × 2) | 1 041 / 1 022 / 1 116 | 1 096 / 1 066 / 1 394 | 795 / 1 100 / 1 986 | −27 / +3 / **+42 %** |
| Bytes per entry (100 000) | 94.4 | 94.4 | 338.9 | ×3.6 |

Put at 100 000 (CPU profile, 2 000 000 puts): index `Remove` 31% and `Add` 22% of put time — deletes and inserts in
buckets that hold tens of thousands of keys (one tail such as `list`); the 5 random slot reads are most of the rest.
Against the S0 column (recorded when HEAD measured 10–30% faster): Get +12 / +17 / +6 %, put −5 / +28 / +85 %.

## S4 — slow-consumer kicks (measured only; no default changed)

`testing/load/kicks_test.go` (`REDLITE_LOAD_TEST=1`; skipped otherwise): 3 readers on one sheet hold a fixed rate of
Sets for 3 s (a third each, 16 workers per reader); every Set is broadcast to the other two. Counted from the server's
log: "Sheet: collaborator kicked", "Sheet: joined" after the first 3 (= resent sheet_state). Under `-race`.
`out/s4_load.txt`:

| `REDLITE_CONN_SEND_QUEUE` | 50 writes/s | 500 writes/s | 5 000 writes/s |
|---|---|---|---|
| 256 (default) | 49/s · 0 kicks · 0 resent · 0 errors | 500/s · 0 · 0 · 0 | 4 999/s · 0 · 0 · 0 |
| 1 024 | 50/s · 0 · 0 · 0 | 498/s · 0 · 0 · 0 | 4 999/s · 0 · 0 · 0 |
| 4 096 | 49/s · 0 · 0 · 0 | 500/s · 0 · 0 · 0 | 4 983/s · 0 · 0 · 0 |
| **8 (control)** | | | 4 989/s · **180 kicks · 65 resent · 8 685 not connected** |

The control proves the counters see kicks. **Recommendation: keep 256.** No cell up to 5 000 writes/s on one sheet
kicked anyone; the audit's 420–1 182 kicks per 2 s came from `TestMultiReaderConcurrentWrites`' tight loop (~140 000
ops/s, mostly deletes), far past any rate backendAdvo produces. Raising the queue only buys tolerance of longer reader
stalls, paid in memory per connection (queue length × frame size); revisit if production ever shows kicks.

## Checker

Fresh `general-purpose` subagent; inputs: S1–S3 criteria and the owned-path diff only. It re-ran every criterion
(harness for tests, interleaved HEAD/current benches) and wrote nothing in the repo.

| Criterion | Verdict | Its evidence |
|---|---|---|
| S1 | PASS WITH NOTE | both counter tests pass; harness covers every StartOn server and NewReaderAt reader; gaps: v1compat readers, reader unit tests built with `mustNew`, panics net/http recovers itself (REST, `/replica`, `/health`, ServeHTTP before `c.Run`), no end-to-end `App.Stats().Panics` test |
| S2a | PASS WITH NOTE | 4 seeds green; full key-set comparison every 1 000 ops; model has no eviction or sheet_state |
| S2b | PASS | prefix 1.42×, suffix 1.33× (its run) |
| S2c | **FAIL (put at 100 000)** | Get 0.99 / 0.99 / 0.96 of HEAD; put 0.72 / 0.94 / **1.44**, 1 → 4 allocs/op |
| S2d | PASS | L13 and drain tests; `internal/cache` 52/52, `store_test.go` untouched |
| S2e | PASS WITH NOTE | 94.43 → 338.9 B/entry ≈ +24 MB per reader at the default 100 000 cap |
| S3 | PASS WITH NOTE | 100/100 ×4; 0/100 on the pre-change copy; `lookup` RLock only |

Its review findings and what happened:
1. MEDIUM — put / memory cost of the index (S2c, S2e): **not fixed**, decision needed.
2. MEDIUM — 1 s access resolution inverted LRU at a 1 ms insert step (0 of 100): **fixed** — test made per step
   (red at 1 ms, then green), resolution 100 ms after 1 ms cost Get +22% at 100 000 (S3 table).
3. MEDIUM — S1 coverage gaps above: not fixed (outside the two guards the request named) → FOUND.
4. LOW — the expired-entry search fell from 64 entries to a sample of 5: expired entries linger longer in memory
   (still misses, R4) → FOUND.
5. LOW — `slots` keeps its peak capacity until the map empties (freed slots are zeroed) → FOUND.
Checked fine: lock order, atomic vs plain `access`, swap-remove on every path, R2/R3/R4/R6/D-008, the
`internal/cache` move. Its full suite: `366 | 351 | 0 | 15`.

## FOUND, NOT FIXED

1. Put on a full local map at 100 000 entries is +43% vs HEAD (criterion S2c) — decision needed (final report).
2. Panics on replica-stream connections are not counted: `internal/replica/leader.go:74` builds its `ws.Conn` without
   `ConnOptions.Panics` (outside the owned paths).
3. `reader/v1compat` readers expose no `Stats`, so their reader-side panics are not checked; the server side is
   (`testing/conformance/v1compat_test.go:27`).
4. `doc/protocol/wire.md:124` lists the `/health` stats fields without `panics` (compatibility doc, not edited).
5. `examples/README.md` still marks `doc/reference/lru_cache.md` "not started" (examples/ is read-only here).
6. `doc/CHANGELOG.md` (2026-10-29 leak-suite entry) recommends "D-043" for replica revoke F-1; D-043 is now this
   decision, so that recommendation needs another id when it is logged.
7. Panics net/http recovers itself (REST API, `/replica`, `/health`, `ws.Handler.ServeHTTP` before `c.Run`) are logged
   but not counted; no test checks `App.Stats().Panics` end to end (checker).
8. Eviction's expired-entry search is 5 draws instead of 64 entries: expired entries stay in memory longer until the
   janitor's invalidate frees them (reads still miss). `slots` keeps its peak capacity until the map empties (checker).
10. `internal/persist/writer_test.go:493` `TestRevokeDiscardsHeldBatch` (not touched by this work) timed out once
    ("timed out waiting for first attempt failed", 3 s) in a full run (`testing/runs/20261110_125545__.log`), then
    passed alone twice (0.12 s) and in the next full run (`20261110_125636__.log`): a 3 s wait that is tight under
    full-suite `-race` load.
9. `reader/stats_test.go:137` was edited (outside the declared paths): it compared the whole `Stats` struct and the new
   bucket fields are correctly nonzero for its 2 keys.
