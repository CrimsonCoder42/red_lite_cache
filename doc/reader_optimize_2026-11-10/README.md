# Reader local-map optimisation (2026-11-10, D-043)

Panic counters on both sides (S1), the server's key index in the reader's local map (S2), sampled-LRU
eviction (S3), and a measurement of slow-consumer kicks (S4, nothing changed).

| File | What |
|---|---|
| [report.md](report.md) | Per section: what changed (file:line), tests, measured numbers with their run, checker result, FOUND / NOT FIXED |
| `out/bench_before.txt` | S0: `BenchmarkLocal*` on HEAD + the audit test files, count=6 |
| `out/bench_after_s2.txt` | After S2 (index), before S3 |
| `out/bench_after.txt` | Final, count=6 |
| `out/bench_head_rerun.txt` | HEAD again, run back to back with `bench_after.txt` (same machine state) |
| `out/bench_store_before.txt`, `bench_store_head_rerun.txt`, `bench_store_after.txt` | Server `internal/cache` benches around the index move |
| `out/s4_load.txt` | S4 load matrix (`TestLoadKicks`) |

Run: `bash testing/run.sh reader testing/leaks` · benches `go test -run='^$' -bench=BenchmarkLocal -benchmem -count=6 ./reader/` ·
load `REDLITE_LOAD_TEST=1 bash testing/run.sh testing/load`.
