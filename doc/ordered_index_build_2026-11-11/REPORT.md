# S9 steps 1, 2, 3, 5 — build report (2026-11-11)

What changed:
- **Index:** the server store and the reader's local map find and clear groups through one shared
  structure, `internal/cache.Groups`: forward and reversed B+trees (D-044). The root/tail/head
  index and the `patient` segment are gone.
- **Expiry:** expiry runs off `internal/cache.Wheel` on both sides (D-045).
- **Eviction:** stays sampled LRU (D-046).
- **Wire:** no change. `protocol/` and `doc/protocol/wire.md` are untouched; `reader/v1compat` is unchanged.

## Harness

- **Before:** `== Total | Passed | Failed | Skipped : 369 | 354 | 0 | 15`, 0 DATA RACE
  (`testing/runs/20261111_100010__.log`, taken before the first edit).
- **After:** see `out/harness_after.txt`. Touched packages after the checker fixes:
  `216 | 215 | 0 | 1`, 0 DATA RACE.
- **Re-runs:** none needed.
- **One red during the build, a test defect fixed:** `TestL11ReplicaStepDownFrees` compared the
  whole store Stats of two nodes. B+tree node counts depend on build order (snapshot bulk-load vs
  key by key), so it now compares entries (`testing/leaks/e2e_test.go:408-410`).

## Machine, method, drift

- **Machine:** Apple M1 Pro, 10 cores, 16 GB, Go 1.27.3 darwin/arm64.
- **Before:** the starting tree, i.e. the working tree as found (with the uncommitted 2026-11-10
  reader work), copied before the first edit.
- **Interleaved run:** six rounds, 10:20–10:33. Each round ran the prototype
  (`doc/project_sheets_2026-11-11`'s probe, in the starting-tree copy), then `BenchmarkGroups*`,
  then the starting tree and this build for internal/cache, reader, internal/sheet and conformance
  B1.
- **Expiry benchmarks:** six more interleaved rounds after the checker fixes (`out/cmp_expire_*`).
- **Drift:** rounds 4–6 overlapped the checker's harness runs, so the spreads are wider there
  (e.g. GroupsMixed writes/s 47k–95k).
  - The prototype re-run measured 5–19% slower than its own spec numbers from the morning. So
    `Groups` is compared with both: the spec target, and the prototype interleaved.
  - One quiet single round (`out/groups_ratio_quick.txt`) repeats the one-match deletes.

## Metric table (medians of 6; 100 000 keys unless stated)

| Metric | Before | After | Spec target | Met? |
|---|---|---|---|---|
| Groups one-match prefix delete, colon / path | prototype (interleaved) 2.26 / 2.07 µs | 2.31 / 1.93 µs (quiet round 2.14 / 1.65) | ≤ 2.15 / 1.74 µs | yes within 15% (+7% / +11%); quiet round at target |
| Groups one-match suffix delete, colon / path | 1.76 / 2.30 µs | 1.86 / 2.23 µs (quiet 1.72 / 2.04) | ≤ 1.67 / 2.18 µs | yes within 15% (+12% / +2%); quiet +3% / at target |
| Groups scan of 100 keys | 0.93 / 0.81 µs | 0.89 / 0.73 µs | ≤ 0.91 / 0.76 µs | yes |
| Groups new key on a full map | 2.72 / 2.57 µs | 2.60 / 2.45 µs | ≤ 2.41 / 2.32 µs | yes within 15% (+8% / +6%) |
| Groups heap per entry (map + slots + both trees) | 220.3 / 186.1 B | 217.4 / 184.0 B | ≤ 220 / 186 B | yes |
| Groups build from sorted snapshot | 28.3 / 22.0 ms | 28.0 / 23.1 ms | ≤ 28.8 / 21.9 ms | yes / +5% |
| Groups mixed: Get under 16 writers | 348 / 341 ns | 353 / 325 ns | ≤ 330 / 301 ns | yes within 15% (+7% / +8%) |
| Groups parallel Get | — | 130 / 124 ns | ≤ 165 ns | yes |
| One-match delete 100k ÷ 10k (criterion 3) | — | prefix 2.2 / 1.9, suffix 2.0 / 2.2 | ≤ 3× | yes |
| Store Expire, nothing due (criterion 7) | 896.6 µs | 0.79 µs | ≤ 1 µs (spec: < 10 µs hold) | yes |
| Store Expire, 10 000 due of 100 000 | 7.20 ms | 7.05 ms | ≤ HEAD | yes |
| Reader parallel local Get, 1k / 10k / 100k | 134.5 / 136.5 / 134.1 ns | 130.2 / 132.7 / 134.4 ns | within 10% of start | yes (−3.2 / −2.8 / +0.2%) |
| Reader bytes per entry | 338.9 B | 270.4 B | — | −20% |
| Reader put on a full map, 1k / 10k / 100k | 776 / 1034 / 1939 ns | 687 / 1016 / 1972 ns | ≤ +39% (decision 6) | yes (−11 / −2 / +2%) |
| Reader one-match prefix delete, 1k / 10k / 100k | 566 / 619 / 832 ns | 657 / 777 / 1008 ns | — | +16 / +26 / +21% (the head index was built for this shape) |
| Reader one-match suffix delete, 1k / 10k / 100k | 536 / 524 / 737 ns | 628 / 772 / 1077 ns | — | +17 / +47 / +46% (spec §2: `:`-aligned suffix is the B+tree's weak case) |
| Store Set (overwrite), store GetParallel | 50.7 / 151.2 ns | 62.8 / 142.9 ns | — | +24% (spread 51–75) / −5.5% |
| Store DeletePrefix of a 2 000-key root; miss | 595 µs; 41 ns | 655 µs; 77 ns | — | +10%; +85% (tens of ns) |
| B1 profile sweep / miss / churn (conformance corpus) | head index 8.7 µs / 447 ns / 451 ns | 11.0 µs / 61 ns / 454 ns | — | +26% / −86% / level |
| Sheet Apply set, 3 collaborators | 715 ns | 791 ns | — | +11% |
| Wheel compaction, 100 000 keys (one day / one second) | — | 12.1 / 8.2 ms (6 runs, not interleaved) | spec estimate ≤ 28.8 ms | yes |

Raw output and medians:
- `out/medians_proto.txt`, `out/medians_groups.txt`;
- `out/compare_start_build.txt` (built from `out/medians_{cache,reader,sheet_b1}_{start,build}.txt`);
- `out/medians_sweep.txt`.

## Step 1 sweep (node size / merge threshold, 100 000 keys, medians of 6)

Results are in `out/medians_sweep.txt`:
- **Memory:** 128/32 and 128/64 lowest (217.4 / 184.0 B/entry); 64/16 is 221.3 / 186.8, 16/4 is 246.5 / 202.0.
- **Scan of 100 keys:** 128-key nodes fastest (790–811 / 737–751 ns) vs 838 / 787 at 64/16.
- **Put and prefix delete:** within ±5% across 32–128.

Chosen: **128 / 32** (`internal/cache/groups.go:27-30`).

## What each new test did on the old code (`out/oldcode_*.txt`)

| Test | Criterion | Starting tree | This build |
|---|---|---|---|
| `TestGroupsMatchNaive`, `TestGroupsEdgeCases`, `TestPrefixEnd`, `TestBulkBuildEqualsIncremental`, `TestWheelDueExact`, `TestWheelCompaction`, `TestReplicaWheelOnPromotion` | 1, 4 (bulk), 8, 9 | build failed: the types do not exist | PASS |
| `TestNoConsumerShapes` | 5 | FAIL: `index.go` and `reader/reader.go` name `patient` | PASS |
| `TestWheelIdlePassHoldTime` | 7 | FAIL: 9.33 ms at 100k vs 98.8 µs at 1k (-race) | PASS: 4.2 µs vs 3.1 µs |
| `TestReaderDropsExpiredOffline` | 10 | FAIL: expired entries never dropped (timeout) | PASS |
| `TestWheelWallClockJump` (checker defect 1) | 6, 9 | — | FAIL before the fix (key stranded), PASS after |
| `TestWireGroupsAnyBoundary` | 2 | PASS: behaviour kept, a regression guard | PASS |
| `TestWireExpiryBroadcast` | 6 | PASS (2.03 s after a 2 s TTL): a regression guard | PASS (2.02 s) |
| leaks `TestL15GroupsDrain` | leak rule | — (no wheel or nodes before) | PASS |

**Black-box over the wire:** `TestWireGroupsAnyBoundary` and `TestWireExpiryBroadcast` (reader's
external test package, v1 frames only). Both run unchanged against any server at an address.

## R4, R5, R7 now

- **R4:**
  - `reader` `TestR4ExpiryAndBound` (unchanged, green);
  - `TestReaderDropsExpiredOffline` (an expired entry is a miss before the local pass drops it);
  - `TestEvictionKeepsHotKeys` (green at 1 s / 10 ms / 1 ms, ≥ 95/100).
- **R5:**
  - integration `TestKeyLoop`, `internal/sheet` `TestRunJanitor` (unchanged, green);
  - `TestWireExpiryBroadcast` (invalidate{keys} over the wire);
  - `TestReplicaWheelOnPromotion`.
- **R7:**
  - `TestGroupsMatchNaive` (200 000 ops × 2 shapes × 2 node sizes);
  - `TestWireGroupsAnyBoundary`;
  - the criterion-3 ratio above;
  - `reader` `TestLocalIndexMatchesScan` (green).

## Checker (fresh subagent; diff of the owned paths + the spec's criteria)

| Criterion | Verdict | Notes |
|---|---|---|
| A index = naive scan, both shapes | PASS | Own randomized test: node sizes 4/5/8/128, bytes 0x00/0xff, Load, drain |
| B no consumer shape in reader/internal/protocol/cmd | PASS | Test files in `internal/sheet` still had `patient`: renamed |
| C locks | PASS | No new lock, no inversion. `Store.mu` and `local.mu` guard the index and wheel; `replace` builds outside the lock |
| D hold times | PASS WITH NOTES | Compaction took 50 ms. Rewritten in place: 8–12 ms. The Debug log line is added. Idle-pass test now spreads expiries over a day, so the walk branch is timed |
| E never serves an expired value | PASS WITH NOTES | **Defect 1 (fixed, red first):** a wall clock stepped ahead of the monotonic clock stranded a key behind the walk (never removed or broadcast; reads still missed). Now decided on the wall clock: `TestWheelWallClockJump` |
| F one implementation | PASS | Reader imports `cache.Groups` / `cache.Wheel`; no copy |
| G harness on touched packages | PASS | 213 / 212 / 0 / 1, 0 DATA RACE (before the fixes); 216 / 215 / 0 / 1 after |

**Also fixed from the checker:**
- `Registry.Stats` now sums `WheelItems`/`WheelSlots`.
- `TestBulkBuildEqualsIncremental` and leaks `TestL15GroupsDrain` are added.
- The comment on `reader.Stats.Entries` is corrected.

## FOUND, NOT FIXED

- **`protocol.HealthStats` keeps `root_buckets`/`tail_buckets`/`head_buckets`.**
  - `/health` (development only) now fills the first two with node counts and the third with 0 (`internal/app/app.go:184-186`).
  - Renaming them is a protocol change outside this build. `doc/protocol/wire.md:124` still says "index buckets".
- **Wheel physical removal is late by a backward wall-clock step.** Removal and the broadcast wait the size of the step; reads still miss on time (D-045).
- **The reader does not log its wheel compactions.** The `local` map has no logger.
- **`internal/cache/groups_bench_test.go`:** `BenchmarkWheelCompact` was appended with a shell heredoc instead of the Edit tool (content checked, gofmt-clean).
