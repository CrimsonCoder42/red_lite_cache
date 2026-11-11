# ordered_index_build_2026-11-11 — S9 steps 1, 2, 3 and 5

Build of `doc/specs/s9_ordered_structures.md` steps 1 (ordered structure), 2 (server Store), 3
(reader local map) and 5 (expiry wheel). Devin's decisions 1–6 of 2026-11-11 (D-044, D-045, D-046).
Not in this build: step 4 (sorted export, ships with the new wire), step 6 (per-sheet TTL), step 7
(collections).

| File | What |
|---|---|
| `PROGRESS.md` | Step-by-step status, updated after every step |
| `REPORT.md` | Evidence: harness lines, metric table (before / after / spec target / met?), node-size sweep, red-first record |
| `out/bench_baseline_start_tree.txt` | Benchmarks on the starting tree before the first edit (count=6) |
| `out/sweep_r<N>.txt` | Node-size and merge-threshold sweep, round N of 6 |
| `out/groups_r<N>.txt` | `BenchmarkGroups*` (the spec's target set), round N of 6 |
| `out/cmp_<pkg>_<tree>_r<N>.txt` | Interleaved store / reader / sheet / conformance benchmarks: `start` = the starting tree, `build` = this build |
| `out/medians_*.txt` | Medians, n, min, max per line |
| `out/oldcode_*.txt` | The new tests run against the starting tree (what each did on the old code) |
| `out/harness_*.txt` | Harness lines before and after |

The starting tree is the working tree as found at 2026-11-11 10:00 (it includes the uncommitted
2026-11-10 reader optimisation), copied before the first edit and used for every "before" number.
