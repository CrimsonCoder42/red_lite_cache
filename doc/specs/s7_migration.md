# S7 backendAdvo migration — Spec

**Status:** planned (evidence built 2026-10-29: `testing/conformance/`, `reader/v1compat`)
**Reference:** `advo_cache/reader/reader.go` (the surface v1compat reproduces); backendAdvo `cache/store.go`, `cache/shared.go`, `cache/keys.go`, `cmd/api/main.go:112-120`, `util/config.go:19-24`. Plan: `doc/consumer/backendadvo_migration.md`.

## Goal

backendAdvo swaps advo_cache for red_lite_cache and deletes the workarounds the library now makes unnecessary, one step at a time, each reversible.

## Acceptance criteria (the cut-over steps)

1. Step 1: backendAdvo imports `red_lite_cache/reader/v1compat`, pins v0.3.0, drops the `../advo_cache` replace; `curl_tester/run.sh` and `curl_tests/*.sh` green with zero code change beyond the import.
2. Step 2a: DROPPED SWEEPS queue + `linkReady` deleted; an outage drill (leader stopped, write, restart, read) shows no stale read.
3. Step 2b: `secondSweepDelay` deleted; write-then-read invalidation phases green.
4. Step 2c: boot no longer Fatal when the cache is down (only on ErrAuthFailed); the API serves from the database.
5. Step 3: `Store.Get` is `reader.Get(key, target)`; v1compat dropped; harness green.
6. Step 4 (optional): `REDLITE_ADMISSION=key` with `Config.APIKey`.

## Surface

| Kind | Name | Package |
|---|---|---|
| package | `reader/v1compat` (advo_cache's reader surface over `reader.Reader`) | reader/v1compat |
| tests | C1–C13, B1, edge cases | testing/conformance |

## Value-source table

| Value | Source | Decided |
|---|---|---|
| reader addresses, sheet, password | `CACHE_LEADER_ADDR`, `CACHE_BACKUP_ADDR`, `CACHE_SHEET`, `CACHE_PASSWORD` (`util/config.go:19-24`) | ✅ D-036 |
| admission on the cache | `REDLITE_ADMISSION=password` until step 4 | ✅ D-024 |
| cluster token | `REDLITE_CLUSTER_TOKEN`, same on both cache nodes | ✅ D-031 |

## Failure behavior

| Failure | What the caller sees | What the system does |
|---|---|---|
| Socket down | reads miss; deletes return nil (queued) | reconnect forever; replay before reads resume |
| Read-through raced a sweep | `Set` returns nil (v1compat) / `ErrStale` (reader) | value not cached |

## Reuse

`testing/integration` harness; `reader` public API; `doc/release.md` runbook for creating the sheet.

## Decisions

D-032…D-037; D-016 (typed Get); D-023 (R6 miss window); D-024 (admission).

## Wire compatibility

No wire change in this slice.

## Out of scope

Editing backendAdvo from this repository (read-only); the advomedix deploy workflows.

## Tests

| Criterion | Evidence here | backendAdvo gate |
|---|---|---|
| 1 | `TestC12V1CompatSurface`, `TestC12V1NewAgainstNothing`, `TestC12V1MaxReconnectAttemptsIgnored` | `curl_tester/run.sh`, `curl_tests/*.sh` |
| 2 | `TestC8DisconnectRetiresDroppedSweeps`, `TestC9BootWhileCacheDown` | `phase-5`, `phase-9`, outage drill |
| 3 | `TestC2WriteInvalidatesEverywhere`, `TestC3ReadThroughRaceNeverRecaches` | `phase-4`, `phase-6` |
| 4 | `TestC9BootWhileCacheDown` | boot with the cache down |
| 5 | `TestR8TypedIsolatedGet`, every `TestC*` | full harness |
| 6 | `TestC*/key_C13` | full harness in key mode |
