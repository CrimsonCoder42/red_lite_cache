# red_lite_cache — Scope

## For / Strategy

> **One embeddable cache client, one leader, zero stale reads across every backend instance.**

`backendAdvo` runs on more than one host and each host kept its own in-memory copy. `advo_cache`
solved that with a WebSocket leader and a reader library. This module is its successor, built as a
proper Go package a consumer can `go get` and pin, with the defects the first version shipped
fixed on purpose (see Already built → Known defects) and persistence added.

**Consumers:** `advomedix-v2/backendAdvo` first (`cache/store.go` wraps the reader). Any other Go
service later. Non-Go clients speak the wire protocol directly (`doc/protocol/wire.md`).

**Non-goals for v1:** a general-purpose Redis replacement, pub/sub beyond invalidation, a web UI.

## Already built (in advo_cache, to be rebuilt here)

Read from `advomedix-v2/advo_cache` on 2026-10-29. Each row names the reference file to copy the
shape from. Status is this repo's status.

| Capability | Reference | Here | Notes |
|---|---|---|---|
| Sheet open/auth (bcrypt) | `sheet/sheet.go:38-82`, `ws/handler.go:222-255` | built (S1/S2) | Keep |
| get/set/delete/delete_prefix/delete_suffix/dump | `ws/handler.go`, `cache/cache.go` | built (S1/S2); `dump` → admin export | `sync.Map` → `map`+`RWMutex` (D-002) |
| TTL + jitter | `cache/cache.go:44-51` | built (S1) | Goroutine-per-entry → lazy expiry + janitor (D-003) |
| Null placeholder (`set_null`) | `cache/cache.go`, `reader/reader.go` | built | Keep |
| Distributed lock (own/TTL/cleanup) | `lock/lock.go` | built (S2) | Keep; not replicated |
| Broadcast replicate/invalidate | `sheet/sheet.go:100-163` | built (S1) | Keep; sender excluded |
| Leader/replica + heartbeat + promotion | `replica/client.go`, `ws/handler.go` | built (S4), fenced | Keep |
| Reader library | `reader/reader.go` | built (S3) | Reconnect forever (D-004); replay queued deletes (D-005) |
| Config via `.env` | `config/config.go` | built | Viper → stdlib env (D-006) |
| Logger | `logger/logger.go` | built | → `log/slog` (D-007) |

### Known defects in the reference, fixed here by design

| # | advo_cache defect | Where | Fix (D-id) | Test (fails on advo_cache, passes here) |
|---|---|---|---|---|
| K1 | Reader's local map has no TTL and no eviction | `reader/reader.go` | D-008 | `reader` `TestR4ExpiryAndBound`; integration `TestKeyLoop` |
| K2 | Deletes dropped while the socket is disconnected | `reader/reader.go` | D-005 | integration `TestDisconnectLoop`; `reader` `TestR2DeletesQueuedAndReplayed`, `TestR2QueueOverflowCollapses` |
| K3 | Server-side TTL expiry is not broadcast | `cache/cache.go` | D-003/D-009 | integration `TestKeyLoop` (invalidate{keys}); `internal/sheet` `TestRunJanitor` |
| K4 | Reader gives up after `MaxReconnectAttempts` | `reader/reader.go` | D-004 | integration `TestReconnectForever`; `reader` `TestR1ReconnectForever` |
| K5 | One goroutine per cached entry for expiry | `cache/cache.go:44-51` | D-003 | `internal/cache` `TestResetExtendsLife`, `TestLazyExpiry` |
| K6 | Full-state sync is one monolithic frame | `ws/handler.go` | snapshot_sheet per sheet | integration `TestSnapshotThenStream` |
| K7 | Replica message types duplicated between server and reader | `replica/replica.go`, `reader/reader.go` | `protocol/` package | `protocol` `TestV1CompatibilityFieldNames` (one definition; the hook rejects untyped messages) |
| K8 | 150-line how-to printed to stdout on every boot | `main.go:95-260` | `logger.Banner`, `doc/protocol/wire.md` | binary run in bootstrap report (one `Config: redlite starting` line) — NO TEST |
| K9 | Server writes to a conn from many goroutines, no mutex → gorilla concurrent-write panic | `sheet/sheet.go:123,145`, `ws/handler.go:264,501,579,620,654` | per-conn send queue / D-011 | integration `TestEdgeConcurrentWritersSeq` (-race); `internal/ws` `TestConnQueueBoundAndKick` |
| K10 | No ordering guarantee between collaborators | `ws/handler.go:284-306` | per-sheet mutex (D-014) | integration `TestEdgeConcurrentWritersSeq`; `internal/sheet` `TestOrderConcurrentApply`, `TestJoinOrderUnderLoad` |
| K11 | Promotion without fencing: old leader never steps down, no epoch → split-brain | `replica/client.go:243-294` | D-012 | integration `TestFencingStepDown`, `TestFencingByReaderEpoch`, `TestEdgeStaleEpoch`; `reader` `TestEpochRefusedFailsOver` |
| K12 | Promoted replica's sheets have no password; first opener sets it (tenant takeover) | `sheet/sheet.go:66-78` | hashes replicate | integration `TestReplicaLoop` (wrong password refused after promotion); `internal/sheet` `TestK12RestoreKeepsHash` |
| K13 | Sheet deleted when the last collaborator disconnects (collides with persistence) | `ws/handler.go:160-164` | D-013 | integration `TestEdgeSheetSurvivesLastLeave`; `internal/sheet` `TestK13LeaveKeepsSheet` |
| K14 | Any client can create any sheet with any password | `sheet/manager.go:27-64` | registry + admin API (S6) | integration `TestAutocreateOffRegistryPath`, `TestEdgeNoAutocreate`; `internal/sheet` `TestK14Autocreate` |
| K15 | No read limit, ping/pong, deadlines or backpressure | `ws/handler.go:64-70` | S2 | integration `TestEdgeOversizedFrame`, `TestEdgeSlowConsumerKicked`; `internal/ws` `TestConnPingFailureCloses` |
| K16 | Zero tests, no `-race`, no benchmarks | repo-wide | every slice ships its loop tests | `bash testing/run.sh` (-race): 159 tests; `make bench` → `doc/bootstrap_2026-10-29/bench_s1.txt` |

Full analysis with the pattern comparison: `doc/assessment_2026-10-29.md`.

## Slices

| # | Slice | Spec | Status | Depends on |
|---|---|---|---|---|
| S0 | Repo scaffold, `.claude`, doc, testing harness | — | **done 2026-10-29** | — |
| S1 | `protocol/` + `internal/cache` + `internal/sheet` (single node, no WS) | `specs/s1_core.md` | **built 2026-10-29** (bootstrap) | S0 |
| S2 | `internal/ws` + `internal/lock` + `internal/app` + `cmd/redlite` (single leader; per-conn send queue, read limit, ping/pong, per-sheet sequencer) | `specs/s2_server.md` | **built 2026-10-29** (bootstrap) | S1 |
| S3 | `reader/` (the embeddable client, K1/K2/K4 fixed) | `specs/s3_reader.md` | **built 2026-10-29** (bootstrap) | S2 |
| S4 | `internal/replica` (heartbeat, snapshot+stream, **fenced** promotion, credentials replicated) | `specs/s4_replica.md` | **built 2026-10-29** (bootstrap) | S2, D-012 |
| S5 | `internal/persist` + `db/migration` (write-behind, hydration, cleaner; sheet lifecycle decoupled from connections) | `specs/s5_persist.md` | **built 2026-10-29**; DB tests SKIPPED (no `REDLITE_TEST_DATABASE_URL`) — not verified against Postgres | S1, D-013 |
| S6 | `internal/api` — the registry: create sheet/project, issue API keys, export. **Required for multi-project** (K14) | `specs/s6_api.md` | **built 2026-10-29** (bootstrap; import and key revocation in TODO) | S5 |
| S7 | `backendAdvo` migration: swap `advo_cache/reader` for this reader | (in advomedix-v2) | not started | S3, tag v0.1.0 |

Order: S1 → S2 → S3 → (S4 ∥ S5) → S6 → S7. A thin tracer bullet (open a sheet, set, get from a
second reader) ships at the end of S3 and is the first tag.

## Pending decisions (ask before S1)

- D-002 `sync.Map` vs `map`+`RWMutex` for the sheet store.
- D-003 Expiry: lazy-on-read + janitor interval.
- D-010 Protocol versioning: a `version` field on `open`, or a path (`/ws/v1`).
- D-019…D-022 Postgres role, secrets in Go, NOTIFY control bus, election mode — `doc/persist/postgres_role.md` §3.
- D-012 Promotion fencing: epoch carried in-protocol (every replicate frame + reader `open`) vs a Postgres advisory lock deciding the leader. `doc/assessment_2026-10-29.md` §3.3.
- D-013 Sheet lifecycle: registry-owned, hydrated from Postgres, evicted by policy — never by "last collaborator out". §3.6.
- D-014 Per-sheet sequencer: one confinement goroutine per sheet (apply + broadcast in order) vs a mutex around apply+broadcast. §2 Ordering.
- D-011 WebSocket library: coder/websocket (ctx-native, concurrent-write-safe, zero deps; recommended) vs
  gorilla/websocket (advo_cache's choice; needs a write pump and manual ping/pong). `doc/reference/codeheim_patterns.md` § go_75.

## Reference knowledge

`doc/reference/README.md` indexes the checked outside sources; `.claude/CLAUDE.md` → "Reference sources"
says how to use and cite them. Distilled so far: Anthony GG's distributed cache stream, 46 code-heim repos.
Pending: `lru_cache.md` (Build Your Own Cache transcript), `advo_cache.md` (file:line walkthrough of the predecessor).
