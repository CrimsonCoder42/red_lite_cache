# docker compose up → a ready cache — report (2026-11-11)

**The command:** `make up` = `mkdir -p .redlite && docker compose up -d --build && docker compose wait
bootstrap`. Plain `docker compose up -d` works the same on macOS.

**What it starts:**
- **postgres:** 127.0.0.1:5434, volume `redlite_pg`; `redlite_test` is created on a fresh volume.
- **leader:** 127.0.0.1:8095, persistence on, key admission, `/health` healthcheck.
- **bootstrap:** one-shot; writes `./.redlite/dev-project.env`, mode 600.
- **replica:** `--profile cluster` only, 127.0.0.1:8096.

On the developer stack it went from nothing to ready in 9 s.

**Port change (Devin's choice, 2026-11-11):** host port 5433 was held by a non-Docker listener on this
machine (a native PostgreSQL), so postgres defaults to **5434**. It can be overridden with
`REDLITE_HOST_PG_PORT`.

## Smoke test (`testing/compose_smoke.sh`, last run; `out/smoke.log`)

```
smoke: up (project redlite_smoke, ports 18095/18096/15433)
smoke: stack up and bootstrapped in 18 s
smoke: /health ok, role leader
smoke: key file present (mode 600), key accepted
smoke: no key, password or admin key in the logs
smoke: second bootstrap: idempotent (1 project)
smoke: failed bootstraps: non-zero exit with a reason, nothing created
smoke: lost key file: recovered with a new key, still 1 project
smoke: reader round trip + leader restart (testing/compose, gated test)
smoke: reader signed in with the key, set/get/clear ok, reconnected after the leader restart, key restored from PostgreSQL
smoke: down -v (project redlite_smoke only)
smoke: PASS
```

**Clean clone:** the tracked and untracked-not-ignored files (229) were copied to a scratch folder with
a canary `.env`. Running `testing/compose_smoke.sh` there under project `redlite_clone`, ports
28095/28096/25433, gave **PASS**: ready in 23 s, then `down -v` of that project only
(`out/clean_clone_smoke.log`, an earlier version of the script without the failure and recovery
checks).

**Gated test from the harness against the developer stack:** `REDLITE_COMPOSE_TEST=1 bash testing/run.sh
--run TestComposeReaderRoundTrip testing/compose` passes. With no key file, which is the state of the
starting tree (no bootstrap), it fails: "bootstrap key file: … no such file".

## Harness

- **Baseline:** `388 | 373 | 0 | 15` (`testing/runs/latest.json` 20261111_104418): 14 persistence skips and
  1 load skip.
- **With persistence on:** `REDLITE_TEST_DATABASE_URL=…127.0.0.1:5434/redlite_test`, run once (`testing/runs/20261111_110217__.log`):
  **`== Total | Passed | Failed | Skipped : 389 | 386 | 1 | 2`, 0 DATA RACE.**
  - The persistence tests now run. The 2 skips are the new gated compose test and the gated load test.
  - **The 1 failure is `TestPersistLoop`** (`testing/integration/persist_test.go:110`). It was red twice: once in
    the full run, once alone. The hydrated value is `{"n": 2}` for `{"n":2}`, because `cache_entries.value`
    is `jsonb` (`db/migration/000001_init.up.sql:67`) and Postgres re-serialises jsonb.
  - It is pre-existing: the test never ran before, since no database existed. It is outside this request's
    paths (db/migration), so it is listed under FOUND, NOT FIXED and in TODO.
- No Go product code changed. The only Go file added is the gated test, so the full harness was not run
  a second time.

## Sizes and times (`out/build_*.txt`)

| | |
|---|---|
| Build context, before `.dockerignore` | 83.8 MB: the whole tree, including `_delete/` 33 MB, `testing/runs/` 36 MB, the 9.7 MB `redlite` binary, `.git` 2.3 MB |
| Build context, after | 1.02 MB ("transferring context: 1.02MB") |
| `redlite:dev` (runtime) | 20.4 MB, binary only, user 10001 |
| `redlite-bootstrap:dev` | 14.5 MB, compose-only |
| Cold build (`docker compose build --no-cache`, base images present) | 216 s |
| Warm build | 1 s |
| `up` to ready (developer stack / smoke / clean clone) | 9 s / 18–20 s / 23 s |

`.env` is kept out of the build context. A canary `.env` sat in the clean clone's directory;
`/src` in the build stage held only `Makefile cmd db docker go.mod go.sum internal protocol reader
sqlc.yaml testing`, and a grep of the whole image for the canary found nothing.

## Checker (fresh subagent, own compose project `redlite_check`)

| Criterion | Verdict | After fixes |
|---|---|---|
| 1 no secret in layers or logs | PASS | history and env clean, a canary build proved `.env` is excluded, 0 hits for the key, password or admin key in the logs |
| 2 ports on 127.0.0.1 | PASS | postgres, leader and replica all `127.0.0.1:` |
| 3 bootstrap idempotent | PASS | 3 more runs: same file checksum, 1 project |
| 4 failure visible, nothing half-created | **FAIL** | fixed: see below; smoke now proves both failure paths and recovery |
| 5 stack ready, replica profile healthy | PASS | |
| 6 file review | PASS WITH NOTES | defects fixed below |

**Fixed in `docker/bootstrap/bootstrap.sh` (rewritten):**
1. **A taken sheet name passed as "ready".** A 409 on create is now accepted only if the project's
   own key can export the sheet. The sheet name also defaults to the project's name, because sheet
   names are global.
2. **A stale key file passed as "ready".** An existing key file must name the same project and
   sheet, or the run fails.
3. **Secrets in curl arguments.** The bearer token and request bodies now go through files
   (`curl -K`, `--data @`), never argv.
4. **The key file was sourced.** It is now read as data; names are validated as `[A-Za-z0-9._-]`.
5. **Key-loss windows.**
   - The create timeout is now 30 s.
   - A 201 without `project_id`/`api_key` fails with a reason line.
   - A 409 for a project whose key was lost is recovered by issuing a new key with the admin key,
     never by creating a second project. The smoke test proves this.
   - Write failures give a reason line.

**Fixed elsewhere:**
6. **Linux.** The bootstrap runs as `REDLITE_BOOTSTRAP_UID/GID` (`docker-compose.yml`), and `make up`
   passes `id -u`/`id -g` and creates `.redlite/`.
7. **Smoke on Linux.** The mode check tries `stat -c` first, then BSD `stat -f`.
8. **Smoke log check.** The logs are captured before grepping (the SIGPIPE false negative), and all
   three secrets are checked.
9. **Image tags.** Tags are `${REDLITE_IMAGE_TAG:-dev}`; the smoke test uses `smoke`, so it never
   retags the developer's images.
10. **`.dockerignore`.** The ineffective `!.env.example` line is removed.

## FOUND, NOT FIXED

- **`TestPersistLoop` and jsonb.** A hydrated value's bytes differ from what the client sent
  (`db/migration/000001_init.up.sql:62` vs `:67`). This is out of scope (db/migration); see TODO.
- **Pre-existing volumes.** A volume created before `docker/postgres/` existed has no `redlite_test`:
  run `docker compose exec postgres createdb -U postgres redlite_test`.
- **Key file outlives the data.** `down -v` leaves `.redlite/dev-project.env` with a dead key. The
  next bootstrap stops with "remove … and run again" (by design; README says to remove it).
- **One brief window.** If the bootstrap is killed between the project create returning and the
  `.partial` write (milliseconds), the next run recovers through the issue-key path, so no project is
  left without a usable key.

## Left for Devin

- Real `REDLITE_ADMIN_KEY` / `REDLITE_CLUSTER_TOKEN` / `REDLITE_PG_PASSWORD` in `.env` before exposing anything.
- The jsonb decision (TODO).
- Committing; no git writes were made.
- The developer stack is still running (`docker compose down` to stop).
- The pristine and clean-clone copies are in the session scratchpad (outside the repo).
