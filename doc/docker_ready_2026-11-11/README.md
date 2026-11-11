# docker_ready_2026-11-11 — one command to a running, ready cache

`docker compose up -d` (or `make up`) → postgres + leader (persistence on, schema migrated) + a one-shot
bootstrap that leaves a development project, its key and a sheet in `./.redlite/dev-project.env` (D-047).

| File | What |
|---|---|
| `PROGRESS.md` | Step list |
| `REPORT.md` | Evidence: smoke runs, harness with persistence on, sizes and times, checker, FOUND |
| `out/build_cold.txt`, `out/build_warm.txt` | `docker compose build` output (cold `--no-cache`, then warm) |
| `out/smoke.log` | The final `testing/compose_smoke.sh` run (repo) |
| `out/clean_clone_smoke.log` | The same from a clean copy of the tracked files, own compose project |
| `out/canary_build.txt` | The `build` target built from a context holding a canary `.env` |
