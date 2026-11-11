# Progress — S9 steps 1, 2, 3, 5

| Step | Status | Evidence |
|---|---|---|
| Baseline | done 10:00 | harness `369 / 354 / 0 / 15`, 0 DATA RACE (`testing/runs/20261111_100010__.log`); `out/bench_baseline_start_tree.txt` |
| 1 — `internal/cache` Groups (B+tree, merge on underflow) | done | red: `TestGroupsMatchNaive` build-failed; green with depth 2–5 trees (node 4 and 128). Sweep → 128/32 (`out/medians_sweep.txt`) |
| 2 — server Store on Groups, keyIndex removed | done | `internal/cache/store.go`; `TestNoConsumerShapes` red (index.go, reader.go) → green after step 3; `TestWireGroupsAnyBoundary` green (regression guard: also green on the old code) |
| 3 — reader local on Groups, cache.Index removed | done | `reader/local.go`; `internal/cache/index.go` → `_delete/internal/cache/index.go`; reader tests green incl. `TestLocalIndexMatchesScan`, `TestEvictionKeepsHotKeys` |
| 5 — expiry wheel, server and reader | done | `internal/cache/wheel.go`; criteria 6–10 tests red first (C7 and C10 also red on the old code), green |
| Interleaved benchmarks (6 rounds, all steps) | done 10:33 (+ expiry rounds after the checker fixes) | `REPORT.md` metric table |
| Checker | done | A, B, C, F, G PASS; D, E PASS WITH NOTES. Defect 1 (wall clock) fixed red-first; compaction rewritten (50 → 8–12 ms); registry wheel sums; 2 tests added |
| Docs | done | D-044/D-045/D-046, CHANGELOG, SCOPE, TODO, spec status, architecture |
| Final harness | see `out/harness_after.txt` | |
