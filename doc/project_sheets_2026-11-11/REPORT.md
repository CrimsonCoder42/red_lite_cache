# Evidence for S8 (project sheets) and S9 (ordered structures) — 2026-11-11

The specs are `doc/specs/s8_project_sheets.md` and `doc/specs/s9_ordered_structures.md`. This file
holds the measurements they cite.
- **Raw output:** `out/r<N>_*.txt`.
- **Medians:** `out/medians.txt` (median, n, min, max per benchmark line and unit).
- **Sizes:** 10,000 and 100,000, as the request asked (1,000 was not run).

## 1. Machine, method, drift

- **Machine:** Apple M1 Pro, 10 cores, 16 GB, macOS Darwin 25.6, Go 1.27.3 darwin/arm64. Benchmarks ran in-process; the pool benchmark used loopback WebSockets to an in-process leader (`integration.StartLeader`).
- **Method:**
  - 6 rounds, 09:27–09:42 local time.
  - Each round is one `go test -count=1` process per group: ops (benchtime 300 ms), memory+build (3x), expiry burst (20x), idle/set cost/eviction (1 s), and pool (1 s).
  - Within a round every candidate ran in turn, so candidates and HEAD are **interleaved** six times.
  - `ExpireIdle` with the wheel was added after the rounds and run 6 more times back to back (`out/r<N>_expire_idle.txt`). Its janitor and heap lines therefore have n = 12.
- **HEAD comparison:**
  - The `hash` candidate *is* HEAD's index: the real `internal/cache.Index` (`internal/cache/index.go:221-247`) driven through the same store wrapper.
  - `head_store` is HEAD's real `internal/cache.Store.Expire` (`internal/cache/store.go:146`).
  - Both ran in the same rounds as the candidates.
- **Drift:** none systematic; rounds started every ~2.5 min. Min–max spread is within ±10% on most lines. The wider ones are:
  - single-thread `Get`/`Set` at 100k (max up to 2.2× the median: GC pauses during the 300 ms runs);
  - `ExpireDue/heap` (one 2.85 ms outlier vs a 1.67 ms median);
  - `Mixed` writes/s (−33% to +36% around the median).
  
  Medians are used throughout.
- **Correctness first:** `TestProbeStructuresMatchNaive` ran each candidate × each key set through 60,000 random ops against a naive model, under `-race`. The ops were insert, remove, prefix group of a random length, suffix group of a random length, and 100-key scan, plus bulk build vs incremental. **All 10 subtests PASS** (`out/probe_naive_test.txt`, 4.10 s; re-run after the checker asked for a saved log).
  - It was run with the probe template's command, `go test -race -count=1 -v ./testing/_probe/ -run TestProbeStructures`, with the probe moved back into `testing/_probe` for the run and out again after it (it now lives in `_delete/testing/_probe/`). `bash testing/run.sh` cannot target `testing/_probe`: it appends `/...`, and Go skips `_` directories in patterns.
- **Key sets** (both 10k and 100k; generators in the probe):
  - `advo` = AdvoMedix shapes (`reader/audit_test.go` benchKeys): `<root>:patient:<uuid>:<view>`, and 1 in 5 `<root>:record:<uuid>`, whose one-match suffix is `:record:<uuid>`.
  - `path` = a different shape with no `:`: `org-NN/site-NNN/dev-NNNNNN/m-<metric>`, whose one-match suffix is `/dev-…/m-…`.

## 2. Ordered index candidates (S9.2, S9.3)

**Definitions:**
- `Set` overwrites an existing key (index untouched).
- `SetFull` is a new key on a full map: one sampled eviction and one insert, in both the forward and reversed index.
- `DeletePrefix1`/`DeleteSuffix1` delete a group of exactly one key, then put the key back.
- `Mixed` runs 16 writers (alternating SetFull and a one-match prefix delete + put back) and 16 readers (Get). It reports ns/op per Get, and writes/s.

### 100,000 keys (medians, n=6) — advo / path

| | hash (HEAD) | sorted slice | skip list | **B+tree** | radix |
|---|---|---|---|---|---|
| Get ns | 59.5 / 45.3 | 54.8 / 43.3 | 52.7 / 44.9 | 57.5 / 43.5 | 53.5 / 48.9 |
| GetParallel ns | 149.2 / 151.7 | 148.9 / 151.4 | 150.8 / 150.6 | 150.1 / 150.3 | 152.5 / 145.2 |
| Set ns | 102.5 / 72.8 | 110.4 / 85.0 | 101.6 / 94.3 | 117.4 / 89.0 | 106.1 / 90.2 |
| SetFull µs | **1.74 / 1.80** | 8.42 / 7.05 | 4.64 / 4.71 | 2.41 / 2.32 | 3.01 / 3.29 |
| DeletePrefix1 µs | 41.2 / 1,198 | 18.8 / 18.2 | 3.95 / 3.80 | **2.15 / 1.74** | 2.73 / 2.50 |
| DeleteSuffix1 µs | **1.10** / 1,036 | 30.3 / 18.8 | 2.44 / 3.77 | 1.67 / **2.18** | 2.14 / 2.77 |
| Scan100 µs | — | 1.54 / 1.29 | 5.46 / 4.89 | **0.91 / 0.76** | 9.91 / 7.10 |
| B/entry | 281 / 674 | **187 / 167** | 272 / 252 | 220 / 186 | 299 / 292 |
| BuildSorted ms | 36.8 / 48.1 | **27.2** / 22.3 | 35.6 / 29.9 | 28.8 / **21.9** | 37.4 / 34.3 |
| Mixed Get ns | 764 / 20,610 | 774 / 687 | 516 / 485 | **330 / 301** | 356 / 359 |
| Mixed writes/s | 38.6k / 2.6k | 44.3k / 48.5k | 74.4k / 75.6k | 82.6k / **86.6k** | **83.7k** / 80.5k |

### 10,000 keys (medians, n=6) — advo / path

| | hash (HEAD) | sorted slice | skip list | **B+tree** | radix |
|---|---|---|---|---|---|
| SetFull µs | **1.05 / 0.97** | 1.58 / 1.47 | 2.03 / 2.01 | 1.15 / 1.11 | 1.56 / 1.54 |
| DeletePrefix1 µs | 4.33 / 117.3 | 2.52 / 2.37 | 1.72 / 1.58 | **1.01 / 0.88** | 1.22 / 1.29 |
| DeleteSuffix1 µs | **0.56** / 139.4 | 3.84 / 2.47 | 1.40 / 1.61 | 0.87 / **0.93** | 0.98 / 1.31 |
| Scan100 µs | — | 1.02 / 1.06 | 1.98 / 1.93 | **0.60 / 0.60** | 5.00 / 4.58 |
| B/entry | 289 / 709 | **208 / 189** | 290 / 270 | 242 / 212 | 314 / 316 |
| BuildSorted ms | 2.82 / 4.26 | 2.11 / **1.82** | 2.91 / 2.75 | 2.13 / 1.99 | 2.86 / 3.07 |
| Mixed Get ns | 208 / 2,331 | 210 / 211 | 229 / 231 | 199 / **196** | 208 / 222 |

**Get is not affected by the index.** Get and GetParallel are within noise for every candidate: the
index is off the read path.

**Reading the results:**
- **HEAD's hash index is shape-dependent.**
  - It wins only where a prefix or suffix is a `:`-segment of a known shape: SetFull, and advo suffix deletes.
  - On advo keys a one-match prefix delete averages 41 µs at 100k over the whole key set (4 in 5 patient keys, 1 in 5 record keys). The record keys have no head, so they fall back to their whole root bucket (`index.go:163-168`).
  - On `/` keys every key is its own root and tail, so a one-match delete scans every bucket (`index.go:169-176`, `:193-199`): 1.0–1.2 ms at 100k. Under mixed load, writes collapse to 2.6k/s and Gets wait 20.6 µs behind the write lock.
  - Its memory is 2.4× higher on `/` keys (one bucket map per key: 674 vs 281 B/entry at 100k).
- **B+tree:** best or within ~20% of best everywhere except SetFull (+39% vs HEAD at 100k advo) and advo suffix (+52%). Its 100k/10k ratios for one-match deletes are 1.9–2.4×: logarithmic, flat in practice.
- **Sorted slice:** least memory and fastest build, but every structural write copies O(n). It loses 3.0–18× to the B+tree at 100k (SetFull 3.0–3.5×, prefix 8.8–10.5×, suffix 8.6–18×).
- **Skip list:** loses to the B+tree on every group, range, SetFull, memory and mixed-load row. It ties or edges it on plain Get/Set at 100k advo (52.7 vs 57.5 ns, 101.6 vs 117.4 ns: index off the path, noise).
- **Radix:** close on deletes and mixed load, but scans are 7.7–10.9× slower, and it uses the most memory of the ordered candidates.
- **Build:** the B+tree's bulk build from sorted input (one pass forward, plus one sort of the reversed keys) is 22–54% faster than HEAD's insert-by-insert.

**Decision recorded in S9 §2:** forward + reversed B+tree.

## 3. Expiry and eviction (S9.4, S9.5)

100,000 keys per store; burst = 10,000 due at once.

| | HEAD Store.Expire | bare sweep | min-heap | wheel (1 s) |
|---|---|---|---|---|
| burst pass | 5.01 ms | 1.02 ms | 1.67 ms | **0.50 ms** |
| idle pass (nothing due) | 911 µs | 588 µs | **2.1 ns** | 658 ns (60 empty s) |
| cost per set | — | 106 ns (map write only) | 188 ns | **134 ns** |
| stale items per key after the set benchmark (no compaction) | — | 0 | 62.7 | 88.1 |

HEAD's pass includes index removal and runs under the sheet mutex. The heap and the wheel make the
idle pass negligible, and the wheel is fastest on bursts and sets. Both need compaction (last row).
**Decision (S9 §4):** wheel with compaction at 2 × live.

| Parallel Get with eviction bookkeeping | 10,000 | 100,000 |
|---|---|---|
| sampled LRU (HEAD's pattern, D-043) | **148 ns** | **150 ns** |
| linked list, own mutex | 189 ns | 241 ns |
| linked list under the write lock | 189 ns | 318 ns |

**Decision (S9 §5):** keep the sampled LRU.

## 4. Connection pool width (S8 §4)

32 goroutines send acknowledged sets to 30 sheets through k connections, over loopback to an
in-process leader.

| k | 1 | 2 | 4 | 8 | 16 | 30 |
|---|---|---|---|---|---|---|
| µs per acked set | 13.45 | 8.43 | **7.55** | 7.61 | 7.96 | 8.58 |

The knee is at 4: 2→4 adds 11.6%, and 4→8 adds −0.8%. **Pool default 4 (S8 §4).**

**Caveat:** today a connection holds one sheet, so k connections were k sheets with the 30 logical
sheets mapped onto them. The measurement is of connection parallelism, which is what a v2 pool
provides; it is not of a v2 server.

## 5. What was not measured

- **Not measured:**
  - a v2 server (it does not exist);
  - per-sheet sub-queues;
  - the B+tree with merge-on-underflow (the prototype leaves empty leaves; long churn could grow memory);
  - node sizes other than 64;
  - wheel compaction cost;
  - collections (S9 §6);
  - `REDLITE_PROJECT_MAX_SHEETS` and the `clear` group cap (both OPEN in S8).
- **⚠️ Not yet tested** against Postgres: the proposed migration `000003_project_sheets.proposed.sql`.

## 6. Harness

- **Before:** `== Total | Passed | Failed | Skipped : 369 | 354 | 0 | 15`, 0 DATA RACE (`testing/runs/20261111_092223__.log`).
- **After:** see `out/harness_after.txt`.
