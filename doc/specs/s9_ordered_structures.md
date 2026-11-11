# S9 — Ordered structures per sheet (server and reader) — Spec

**Status:** steps 1, 2, 3, 5 **built** (2026-11-11, `doc/ordered_index_build_2026-11-11/`; open decisions 1–6
answered yes by Devin → D-044, D-045, D-046). Steps 4, 6, 7 not built. Measured on the build (medians of 6,
interleaved): Groups one-match prefix / suffix delete at 100k 2.31 / 1.86 µs (colon) and 1.93 / 2.23 µs (path), a quiet
round 2.14 / 1.72 and 1.65 / 2.04 µs; scan 0.89 / 0.73 µs; heap 217.4 / 184.0 B/entry; idle expiry pass 0.79 µs; node
size chosen 128 / merge 32 (step-1 sweep). Full table: `doc/ordered_index_build_2026-11-11/REPORT.md`.
**Reference:** new. Replaces the root/tail/head index (`internal/cache/index.go`, D-037) and the
janitor's full sweep (`internal/cache/store.go:146`, D-003). Evidence: `doc/project_sheets_2026-11-11/REPORT.md`
(medians of 6, raw output in its `out/`).
**Companion:** `doc/specs/s8_project_sheets.md`.

## Goal

Every sheet has the same five structures on the server and in the reader. They are general for any
key shape, with no segment names and no separator assumptions:
1. a hash map for exact get;
2. an ordered index for prefix groups;
3. an ordered index over reversed keys for suffix groups;
4. an expiry wheel;
5. sampled-LRU slots.

One Go package holds them and both sides import it. The Rust server implements the same rules,
stated below.

## Rules a second implementation must follow

- **Order** is lexicographic by **bytes** (Go string compare, Rust `[u8]` `Ord`). UTF-8 is not
  interpreted.
- **Prefix group of `p`** (`p` ≠ ""): every key `k` with `p ≤ k < prefixEnd(p)`.
  - `prefixEnd(p)` drops trailing 0xFF bytes, then increments the last byte.
  - If `p` is all 0xFF, there is no upper bound.
  - Pattern A of the request. This is exactly "k starts with p".
  - `p` = "" is the whole sheet (a clear, no index walk).
- **Suffix group of `s`** (`s` ≠ ""): every key whose byte-reversal is in the prefix group of `reverse(s)`.
  - This is exactly "k ends with s", for **any** `s`, segment-aligned or not.
- **A removed group is reported in index order.** It is the order of `invalidate{keys}`-style lists, and the order a replica and a reader apply.

---

## 1. Exact get: the hash map stays

`map[string]entry` under the sheet store's RWMutex (D-002) on the server, and the reader's local map
(`reader/local.go`).

**Measured:** Get is the same for every index candidate, 18–20 ns at 10k and 43–60 ns at 100k
single-threaded, and ~150 ns under parallel Get on 10 cores (report §2). The index never sits on the
read path.

## 2. Groups and ranges: B+tree forward + B+tree reversed

**Recommendation (measured; OPEN 1):** a **B+tree**, 64 keys per node, leaves linked, holding the keys in byte
order. A second B+tree holds the reversed keys. Together they replace `keyIndex` (roots, tails,
heads) and the `patient` special case (`internal/cache/index.go:14-15`, `:33-50`).

| Operation | How |
|---|---|
| insert / remove | descend (log₆₄ n levels), insert into the leaf, split at 65. A production build **merges** a leaf that falls under 16 keys with a neighbour (the prototype did not; see the report §2) |
| prefix group | descend to `p`'s leaf, walk right while keys start with `p`, removing each run with one copy per leaf (patterns A + B): O(log n + matches) |
| suffix group | the same on the reversed tree with `reverse(s)`; each removed reversed key is reversed back and removed from the forward tree and the map |
| range scan | descend to `from`, walk the leaf chain: O(log n + count) |
| build from sorted | bottom-up: leaves filled to 48 (¾), then each parent level from the children's first keys; one pass, no compares (pattern C, the B+tree form of LeetCode 108) |

**Medians, 100,000 keys** (report §2; HEAD = today's `cache.Index`, run in the same rounds):

| | HEAD hash index (advo / path keys) | **B+tree** (advo / path) | skip list | sorted slice | radix |
|---|---|---|---|---|---|
| one-match prefix delete | 41.2 µs / **1,198 µs** | **2.15 / 1.74 µs** | 3.95 / 3.80 | 18.8 / 18.2 | 2.73 / 2.50 |
| one-match suffix delete | **1.10 µs** / **1,036 µs** | 1.67 / 2.18 µs | 2.44 / 3.77 | 30.3 / 18.8 | 2.14 / 2.77 |
| scan of 100 keys | not possible | **0.91 / 0.76 µs** | 5.46 / 4.89 | 1.54 / 1.29 | 9.91 / 7.10 |
| new key on a full map (evict + insert) | **1.74 / 1.80 µs** | 2.41 / 2.32 µs | 4.64 / 4.71 | 8.42 / 7.05 | 3.01 / 3.29 |
| heap per entry (map + slots + index) | 281 / **674 B** | 220 / 186 B | 272 / 252 | **187 / 167** | 299 / 292 |
| build from a sorted snapshot | 36.8 / 48.1 ms | 28.8 / 21.9 ms | 35.6 / 29.9 | **27.2** / 22.3 | 37.4 / 34.3 |
| mixed: Get under 16 writers | 764 / **20,610 ns** | **330 / 301 ns** | 516 / 485 | 774 / 687 | 356 / 359 |
| mixed: writes/s meanwhile | 38.6k / **2.6k** | **82.6k / 86.6k** | 74.4k / 75.6k | 44.3k / 48.5k | 83.7k / 80.5k |

**Why the B+tree wins:**
- It is first or within 20% on every group, range and load row on **both** key shapes, with one exception: advo suffix deletes, where HEAD is 34–36% faster (B+tree +52% at 100k, +55% at 10k).
- Its prefix and suffix cost does not depend on the shape. HEAD is fast only when the prefix or suffix lines up with a `:` segment (advo suffix 1.10 µs).
  - On advo prefixes HEAD averages 41 µs at 100k (19× the B+tree) over the whole key set. Its record keys have no head, so they fall back to the whole root bucket (`index.go:163-168`).
  - It is 475–689× slower on `/`-separated keys, where every key is its own root and its own tail (`index.go:169-176`, `:193-199`, a scan of every bucket).

**What the B+tree loses:**
- A new key on a full map costs +10% to +39% vs HEAD (+10–15% at 10k, +29–39% at 100k): one forward and one reversed tree insert and remove.
- Pure suffix deletes on `:`-aligned keys cost +52% (1.10 → 1.67 µs at 100k advo).

**Why the others lost:**
- **Sorted slice** (with 1,024-op batching): the least memory and fastest build, but every structural write is an O(n) copy. Evict + insert is 8.4 µs and a prefix delete 18.8 µs at 100k, 3.5–8.8× the B+tree.
- **Skip list:** slower than the B+tree on every group, range, full-map and load row (pointer chasing, 8–9 allocations per evict + insert).
- **Radix tree:** close on deletes, but range scans are 7.7–10.9× slower (re-assembling keys) and it uses the most memory of the ordered candidates.

**Memory:** the reversed tree stores a reversed copy of every key, which is included in the
B+tree's 186–220 B/entry. It is still 22–72% less than HEAD's buckets.

**Where it lives (one home):**
- `internal/cache` gains `Ordered` (the B+tree) and `Groups` (forward + reversed + the removal rules), replacing `keyIndex` and `Index` (`internal/cache/index.go:54-247`).
- The server's `Store` (`internal/cache/store.go`) and the reader's `local` (`reader/local.go`) both embed `Groups`. The reader already imports `internal/cache` (doc/specs/s3_reader.md criterion 11).

**Stats:**
- `RootBuckets`/`TailBuckets`/`HeadBuckets` (reader `Stats`, outside the compatibility contract; `/health` stats, D-040, dev-only) become `IndexNodes`.
- `wire.md` § `/health` `stats` changes in the build step that ships it.

**Concurrency:**
- Both trees are guarded by the store's existing write lock (server: store RWMutex inside the sheet mutex; reader: `local.mu`). Get never touches them.
- No new lock and no new goroutine.

## 3. Connect and reconnect: sorted snapshot, one-pass build

**Server:** the v2 `sheet_state` (S8 §6) sends `entries` as `[key, value, expires_in_ms]` in
forward order, by walking the forward tree's leaf chain. That order is free.
- Today `encoding/json` already sorts map keys when it encodes `sheet_state.data`, but the reader decodes into a map and loses the order.
- The v1 `sheet_state` (object form) is unchanged.

**Reader:**
- Appends entries to the map and slots as they arrive, then bulk-builds the forward tree in one pass.
- The reversed tree needs its keys sorted by reversed bytes, which the snapshot order is not. The reader sorts the reversed keys once (n log n).
- The whole build is included in the measured 21.9–28.8 ms at 100k, against HEAD's 36.8–48.1 ms key-by-key (report §2).
- Sending a second, reversed order from the server would double the frame for one sort, so it is rejected.

**Chunks:**
- One join may span several `sheet_state` chunks (S8 §6).
- The reader buffers until `last:true`, then swaps the new structures in under its lock (as `local.replace` does today, `reader/local.go`). Reads never see a half-built index.

## 4. Expiry: a per-second wheel per sheet, in place of the full sweep

**Today:** the one janitor goroutine (D-003) walks **every** entry of every sheet each interval
(`internal/cache/store.go:146-160`), under that sheet's mutex.

**Measured** (report §3; 100,000 keys per sheet):

| | HEAD `Store.Expire` | bare map sweep | min-heap | **wheel (1 s buckets)** |
|---|---|---|---|---|
| pass with 10,000 keys due at once | 5.01 ms | 1.02 ms | 1.67 ms | **0.50 ms** |
| pass with nothing due (every interval of a quiet sheet) | 911 µs | 588 µs | **2.1 ns** | 658 ns (60 empty seconds) |
| added cost per set (keeping the finder current) | — | 0 | +82 ns | **+28 ns** |
| stale items left by overwrites, uncompacted | — | 0 | 62.7 per key | 88.1 per key |

**Recommendation: the wheel (OPEN 3).**
- It is 3.3× faster than the heap on a burst and costs a third as much per set.
- Both turn the idle pass from O(n) into effectively nothing. This is the real gain: today a 100k-key sheet's mutex is held about 0.9 ms every pass, blocking its writes; the wheel holds it under 1 µs.
- **Alternative that lost:** the min-heap (pattern D). It is exact to the nanosecond, but that exactness buys nothing: reads already treat an expired entry as a miss (R4) on both sides, so physical removal may lag by up to the bucket width (1 s) plus the janitor interval with no stale read.

**Rules:**
- **Buckets:** `map[unixSecond][]key`. A set files the key under `floor(expiresAt / 1s)`; an overwrite or delete leaves its old item **stale** (skipped at pass time by comparing the entry's current expiry second).
- **A pass at `now`** processes every whole second in `(last, floor(now/1s) − 1]`. A key is removed only when its live expiry is ≤ now. The one janitor goroutine stays (D-003) and walks the sheets' wheels.
- **Compaction is required:** stale items reach 88 per key (median; 91.5 max) under the overwrite benchmark. When a sheet's wheel holds more than 2 × (live keys with an expiry) items, it is rebuilt from the map: O(n), amortised O(1) per set.
- **Jitter** is applied once, at apply time (`Policy.ExpiresAt`, `internal/cache/policy.go:22`), and carried as an absolute time on the server and as `expires_in_ms` on the wire. The wheel files the jittered time.
- **Replication:**
  - The leader's pass emits `replicate_expire{keys}` and `invalidate{keys}` exactly as today (D-003/D-009; frames unchanged).
  - A replica keeps its wheel current from the frames it applies, but does not run it while not leader (`internal/sheet/janitor.go:26`). On promotion it is already built.
- **Reader:**
  - The same wheel, filled from `expires_in_ms` converted with the reader's clock (D-039).
  - The reader's pass (one goroutine per reader, the read loop's ticker, no new owner) drops expired entries locally without waiting for the server's broadcast. That frees memory while disconnected.
  - An expired entry stays a miss whether or not it has been dropped (R4).
- **Per-sheet policy:** default TTL and jitter percent become per-sheet fields, set by admin and replicated in `replicate_sheet`. They default to `REDLITE_CACHE_TTL` and `REDLITE_CACHE_JITTER_PERCENT`. v2 `set` carries `ttl_ms` (S8 §6).

## 5. Eviction: keep the sampled LRU

**Measured** (report §3, parallel Get on 10 cores):

| | sampled LRU (today, D-043) | doubly linked list, own mutex | list under the write lock (pattern E as written) |
|---|---|---|---|
| 10,000 keys | **148 ns** | 189 ns (+27%) | 189 ns (+27%) |
| 100,000 keys | **150 ns** | 241 ns (+60%) | 318 ns (+111%) |

**Recommendation (OPEN 5):** keep D-043, a 5-draw sample from the dense slot array, with the access stamp stored
atomically under the read lock at most every 100 ms.
- The list makes every Get a write to shared memory (6 pointers), and the cost grows with the map size.
- The list's advantage, exact order, was already met in practice: `reader` `TestEvictionKeepsHotKeys` asserts at least 95/100 hot keys survive (`reader/eviction_test.go:61`); the latest full run kept 97/100 at the 1 ms step (`testing/runs/20261111_094655__.log`).

**On the server:** sheets are bounded by `max keys` (refuse, `sheet full`), not evicted. A per-sheet
eviction mode is out of scope.

## 6. Later: collections sorted by score (design only, not built)

**A collection** is a named, ordered set inside a sheet: `(score int64, member bytes) → value`.
Scores are typically unix-nanosecond timestamps.

**Structure:** the same B+tree, keyed by `bigEndian(score XOR 1<<63) ++ member`.
- That byte order equals numeric order for signed scores, so one structure and one rule serve both keys and collections.
- A member index `map[member]score` makes a re-add O(log n).

| Operation | Cost | Wire (v2 additive, S8 §6 style) |
|---|---|---|
| insert `(coll, score, member, value)` | O(log n) | `cadd{sheet, coll, score, member, value}` → `{seq, pseq}`; broadcast `creplicate` |
| range by score `[min, max]`, `limit`, `rev` | O(log n + count) | `crange{sheet, coll, min, max, limit, rev}` |
| range-delete by score | O(log n + removed): leaf runs, as a prefix group | `cdel_range{sheet, coll, min, max}`; broadcast `cinvalidate{coll, min, max}` |
| count in `[min, max]` | O(log n + leaves); O(log n) with per-node counts (an order-statistic B+tree, chosen at build time by benchmark) | `ccount{sheet, coll, min, max}` |
| newest n across k collections | O(n log k): a heap of one cursor per collection (pattern F, LeetCode 23) | `cmerge{sheet, colls[], n, rev}` |

**Reader:** mirrors a collection like keys (local B+tree, `creplicate` and `cinvalidate` frames, the
same R2 replay and R6 `base_seq` per sheet). `crange` and `cmerge` run locally.

**Decide by benchmark before building:**
- `cmerge` vs a single global collection with a type tag;
- counted vs uncounted nodes.

Targets come from a probe at 10k/100k members, on two score distributions (uniform; bursty).

---

## Acceptance criteria

Verifiable against a running server (**W** = black-box over the wire, any language) or a `-race`
test (**R**).

1. **(R)** `cache.Groups` matches a naive model (map + sort + scan) with zero differences over 200,000 random ops, on **two** key shapes: `:`-segmented, and `/`-paths with no `:`. The ops are insert, remove, prefix group of any length, suffix group of any length, and scan.
2. **(W)** `delete_prefix` with a prefix that ends mid-segment, and `delete_suffix` with a suffix that does not start at a separator, each delete exactly the keys that start or end with it, on both key shapes.
3. **(R)** A one-match prefix or suffix delete at 100,000 keys is at most 3× its time at 10,000, on both key shapes (the R7 rule, now shape-independent).
4. **(W)** v2 `sheet_state` entries are sorted by key bytes; a reader built from them answers every scan and group like one built key by key (R test: the bulk build equals the incremental build).
5. **(R)** No non-test source file under `internal/`, `reader/`, `protocol/` or `cmd/` contains the string `patient`, or a separator constant used for indexing. Test files there that use consumer-shaped keys today are moved to the two generic key sets in build steps 2–3. The AdvoMedix corpus stays only in `testing/conformance`.
6. **(W)** A key set with a 2 s TTL is gone from `get` within 2 s + 1 s (bucket) + `REDLITE_CACHE_JANITOR_INTERVAL`. Every joined session receives `invalidate{keys}` for it.
7. **(R)** With 100,000 keys and nothing due, a janitor pass holds the sheet mutex under 10 µs (today about 0.9 ms).
8. **(R)** Under 100,000 overwrites of 1,000 keys, the wheel holds at most 2 × live items after each pass (compaction works).
9. **(R)** A promoted replica expires keys on schedule without a rebuild pass (its wheel was kept current).
10. **(R)** The reader drops expired entries locally while disconnected (entries count falls) and never serves one (R4).
11. **(R)** Parallel local Get at 100,000 keys stays within 10% of HEAD in an interleaved run (report: ~150 ns).
12. **(R)** Eviction keeps `TestEvictionKeepsHotKeys` green (at least 95/100 hot keys) at every insert step.

## Tests that must exist

| Test | Criterion | Black-box over the wire? | Suite |
|---|---|---|---|
| `TestGroupsMatchNaive` (promoted from the probe's `TestProbeStructuresMatchNaive`, raised to 200k ops) | 1 | no | internal/cache |
| `TestWireGroupsAnyBoundary` | 2 | **yes** | testing/integration (portable list) |
| `BenchmarkGroups*` (Get, GetParallel, SetFull, DeletePrefix1, DeleteSuffix1, Scan100, Memory, BuildSorted, Mixed; 2 key shapes) | 3, targets | no | internal/cache |
| `TestWireSheetStateSorted` | 4 | **yes** | integration |
| `TestBulkBuildEqualsIncremental` | 4 | no | internal/cache |
| `TestNoConsumerShapes` (greps the packages) | 5 | no | testing/conformance |
| `TestWireExpiryBroadcast` | 6 | **yes** | integration |
| `TestWheelIdlePassHoldTime`, `TestWheelCompaction` | 7, 8 | no | internal/sheet |
| `TestReplicaWheelOnPromotion` | 9 | no | integration |
| `TestReaderDropsExpiredOffline` | 10 | no | reader |
| `BenchmarkLocalGetParallel` (exists) | 11 | no | reader |
| `TestEvictionKeepsHotKeys` (exists) | 12 | no | reader |
| leaks `TestL15GroupsDrain`: fill, delete by groups, index nodes back to 0 | — | no | testing/leaks |

## Benchmark targets (become the bar for the Go build and the Rust port)

These are M1 Pro numbers (report §1 machine), medians of 6. A later build must meet them within 10%
in an interleaved run on the same machine. The Rust port uses them as its first bar.

| Benchmark (100,000 keys) | advo keys | path keys |
|---|---|---|
| one-match prefix delete | ≤ 2.15 µs | ≤ 1.74 µs |
| one-match suffix delete | ≤ 1.67 µs | ≤ 2.18 µs |
| scan of 100 keys | ≤ 0.91 µs | ≤ 0.76 µs |
| new key on a full map | ≤ 2.41 µs | ≤ 2.32 µs |
| heap per entry (map + slots + both trees) | ≤ 220 B | ≤ 186 B |
| build from a sorted snapshot | ≤ 28.8 ms | ≤ 21.9 ms |
| mixed: Get under 16 writers | ≤ 330 ns | ≤ 301 ns |
| parallel Get (B+tree medians 150.1 / 150.3 ns; HEAD's index 149.2 / 151.7) | ≤ 165 ns | ≤ 165 ns |
| expiry pass, 10,000 due of 100,000 | ≤ 0.50 ms (bare wheel); full store pass ≤ HEAD's 5.01 ms | — |
| expiry pass, nothing due | ≤ 1 µs | — |

## Value-source table

| Value | Source | Decided |
|---|---|---|
| B+tree node size 64, leaf fill ¾ on build | prototype (report §2); not swept | ❌ OPEN 2 |
| merge threshold 16 keys | standard B+tree half-full rule /2; not measured | ❌ OPEN 2 |
| wheel bucket 1 s | prototype; matches the TTL unit resolution on the wire (ms) only coarsely | ❌ OPEN 3 |
| wheel compaction at 2 × live | not measured | ❌ OPEN 4 |
| eviction sample 5, stamp 100 ms | D-043 | ✅ |

## Failure behaviour

| Failure | What the caller sees | What the system does |
|---|---|---|
| Socket down | unchanged (R3) | the reader's wheel still drops expired entries |
| Sheet not open | unchanged | — |
| Lock held | unchanged | — |
| Persist unavailable | unchanged | expiry still runs from the wheel; the persist cleaner deletes by `expires_at` as today |
| Replica lag | unchanged | replica wheels track applied frames |
| A wheel compaction under a large sheet | a write that triggers it waits O(n) once (≈ the build time, ≤ 28.8 ms at 100k) | amortised; logged at Debug with the size |

## Reuse

- `local.slots` and the sampled eviction (`reader/local.go`, D-043) are kept.
- `Policy.ExpiresAt` (`internal/cache/policy.go:22`).
- The janitor goroutine and its leader gate (`internal/sheet/janitor.go:18-37`).
- `local.replace` swap-under-lock (`reader/local.go`).
- The randomized naive comparison pattern of `reader/index_test.go`.

## Decisions

- **Governing:** D-002 (map + RWMutex), D-003 (one janitor), D-008/D-043 (eviction), D-037 (index by benchmark: this spec supersedes its outcome), D-039 (clock), D-040 (counts).
- **Proposed new D-ids:**
  - ordered B+tree groups (supersedes D-037's head index);
  - the expiry wheel (amends D-003);
  - keep sampled LRU over the list (confirms D-043).

## Wire compatibility

- v1 is unchanged.
- **v2 (S8)** adds `sheet_state.entries` (sorted array) and `ttl_ms`.
- **Behaviour change visible on v1:** `delete_prefix`/`delete_suffix` results are identical (the same key sets; only the speed changes). The `/health` `stats` bucket counts (dev-only, D-040) are renamed in the step that ships it.

## Out of scope

- Server-side eviction.
- Persisting the wheel (rebuilt from `expires_at` at hydration).
- Building collections (§6 is design only).

## Build plan (each step ships and tests alone)

1. **`internal/cache.Ordered` + `Groups`**, with merge-on-underflow, plus `TestGroupsMatchNaive` and the `BenchmarkGroups*` targets. Nothing uses them yet.
2. **Server `Store` on `Groups`** (drop `keyIndex`).
   - Gate: `internal/cache` and `internal/sheet` tests unchanged and green; store benchmarks at the targets.
   - `TestNoConsumerShapes` for `internal/cache`; `internal/cache` tests moved to the two generic key sets.
   - `TestWireGroupsAnyBoundary` (criterion 2: v1 `delete_prefix`/`delete_suffix` over the wire).
3. **Reader `local` on `Groups`** (drop `cache.Index`).
   - Gate: `reader` tests, including `TestLocalIndexMatchesScan` and `TestEvictionKeepsHotKeys`, and `BenchmarkLocalGetParallel` within 10% of HEAD.
4. **Sorted export and bulk build**: the server walks the leaf chain; the reader bulk-builds. Ships with S8's v2 `sheet_state` (S8 build step 4). `TestBulkBuildEqualsIncremental`.
5. **Expiry wheel**, server (janitor walks wheels) and reader (local pass). Tests: criteria 6–10.
6. **Per-sheet TTL/jitter policy** (admin + `replicate_sheet` fields).
7. **Collections** (§6): probe first, then a spec revision.

## What backendAdvo changes (list; no edits there)

1. **Nothing for step 1–5 on v1.** `DeletePrefix`/`DeleteSuffix` keep their meaning and get faster. The `patient` head index going away costs nothing: the B+tree finds `<root>:patient:<uuid>` groups in 2.15 µs at 100k, against HEAD's 41 µs average across that key set.
2. **Suffix clears that were only fast because `:` aligned** (`DeleteSuffix(":permissions")`, `<t>:profile`; `backendAdvo/cache/permissions.go:131-135`, `users.go:428-438`) stay correct for any boundary.
3. **The "sweep twice, 2 s apart" workaround** (`backendAdvo/cache/store.go:300-306`) is already unnecessary under R6. Nothing here changes that; dropping it is the consumer's call.
4. **With S8 v2 and per-sheet TTL:** the per-root TTL constants (`backendAdvo/cache/keys.go:2570-2744`) can become sheet policy.
5. **Later (§6):** timeline-style lists (`readings:…:<cursor>:<size>`, `feed:<s>:<e>`) could become scored collections instead of one cached page per cursor.

## OPEN DECISIONS FOR DEVIN

1. Replace the root/tail/head index with forward + reversed B+trees (both sides)? **yes / no**
2. Accept node size 64 and merge-at-16 untested beyond the prototype, to be swept in build step 1? **yes / no**
3. Expiry by a 1-second wheel (not the exact min-heap), physical removal up to 1 s + interval late, reads unaffected? **yes / no**
4. Compact a wheel when it holds more than 2 × live items? **yes / no**
5. Keep sampled LRU (D-043) and reject the linked list? **yes / no**
6. Accept +10–39% on "new key on a full map" for shape-independent groups and range scans? **yes / no**
7. Scored collections (§6) as the next capability after S8/S9? **yes / no**
