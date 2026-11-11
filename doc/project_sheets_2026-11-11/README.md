# project_sheets_2026-11-11 — project sign-in with many sheets, ordered structures

Spec + prototype benchmarks. No product code changed (`reader/`, `internal/`, `protocol/`, `cmd/`).

| File | What |
|---|---|
| `../specs/s8_project_sheets.md` | S8 spec: access, sheets, pool, order across sheets, wire v2, reader API, R1–R12 |
| `../specs/s9_ordered_structures.md` | S9 spec: per-sheet structures (index, expiry, eviction, sorted snapshot, scored collections) |
| `REPORT.md` | The benchmark evidence behind both specs, with machine and drift |
| `000003_project_sheets.proposed.sql` | Proposed migration for per-project sheet names (not applied; Devin's) |
| `out/r<N>_*.txt` | Raw benchmark output, round N of 6 (one `go test -count=1` process per group per round) |
| `out/medians.txt` | Median, min, max of the 6 rounds per benchmark line and unit |
| `out/harness_*.txt` | Harness lines before and after |

Probes (moved to `_delete/testing/_probe/` at the end; move back to re-run):
`structures_2026-11-11_test.go` (5 index candidates + naive-model test), `expiry_2026-11-11_test.go`
(heap / wheel / janitor, LRU list vs sampled), `pool_2026-11-11_test.go` (connection pool width over
the wire). The probe test runs with the template's command
`go test -race -count=1 -v ./testing/_probe/ -run TestProbe…`: `testing/run.sh` appends `/...`, and
Go skips `_`-prefixed directories in patterns, so the harness cannot target `_probe`.
