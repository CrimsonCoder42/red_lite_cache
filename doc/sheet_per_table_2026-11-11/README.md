# sheet_per_table_2026-11-11 — one project, many sheets: feasibility

Research and measurement only. No change to `reader/`, `internal/`, `protocol/`, `cmd/`.

| File | What |
|---|---|
| `REPORT.md` | The answer: feasibility, option table, recommendation, numbers, build plan, open decisions. |
| `out/round<N>_ops.txt` | Raw benchmark output, round N of 6 (Get, put, deletes, mixed load, server apply). |
| `out/round<N>_conn.txt` | Raw benchmark output, round N of 6 (footprint, connect, reconnect). |
| `out/medians.txt` | Medians of the 6 rounds per benchmark line (built from the raw files). |
| `out/harness_after.txt` | The final `bash testing/run.sh` summary line. |

Benchmark source: `testing/_probe/sheetpertable_2026-11-11_test.go`, moved to
`_delete/testing/_probe/` at the end (re-run it from there by moving it back). Each round is one
`go test -count=1` process, so `sheets=1` and `sheets=30` alternate six times (interleaved).

Consumer facts come from `advomedix-v2/backendAdvo/cache/` (read only).
