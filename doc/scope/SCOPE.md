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
| Sheet open/auth (bcrypt) | `sheet/sheet.go:38-82`, `ws/handler.go:222-255` | not started | Keep |
| get/set/delete/delete_prefix/delete_suffix/dump | `ws/handler.go`, `cache/cache.go` | not started | `sync.Map` → `map`+`RWMutex` (D-002) |
| TTL + jitter | `cache/cache.go:44-51` | not started | Goroutine-per-entry → lazy expiry + janitor (D-003) |
| Null placeholder (`set_null`) | `cache/cache.go`, `reader/reader.go` | not started | Keep |
| Distributed lock (own/TTL/cleanup) | `lock/lock.go` | not started | Keep |
| Broadcast replicate/invalidate | `sheet/sheet.go:100-163` | not started | Keep; sender excluded |
| Leader/replica + heartbeat + promotion | `replica/client.go`, `ws/handler.go` | not started | Keep |
| Reader library | `reader/reader.go` | not started | Reconnect forever (D-004); replay queued deletes (D-005) |
| Config via `.env` | `config/config.go` | not started | Viper → stdlib env (D-006) |
| Logger | `logger/logger.go` | not started | → `log/slog` (D-007) |

### Known defects in the reference, fixed here by design

| # | advo_cache defect | Where | Fix (D-id) |
|---|---|---|---|
| K1 | Reader's local map has no TTL and no eviction | `reader/reader.go` | D-008 |
| K2 | Deletes dropped while the socket is disconnected | `reader/reader.go` | D-005 |
| K3 | Server-side TTL expiry is not broadcast | `cache/cache.go` | D-009 |
| K4 | Reader gives up after `MaxReconnectAttempts` | `reader/reader.go` | D-004 |
| K5 | One goroutine per cached entry for expiry | `cache/cache.go:44-51` | D-003 |
| K6 | Full-state sync is one monolithic frame | `ws/handler.go` | TODO (chunking) |
| K7 | Replica message types duplicated between server and reader | `replica/replica.go`, `reader/reader.go` | `protocol/` package |
| K8 | 150-line how-to printed to stdout on every boot | `main.go:95-260` | `doc/protocol/wire.md` |

## Slices

| # | Slice | Spec | Status | Depends on |
|---|---|---|---|---|
| S0 | Repo scaffold, `.claude`, doc, testing harness | — | **done 2026-10-29** | — |
| S1 | `protocol/` + `internal/cache` + `internal/sheet` (single node, no WS) | `specs/s1_core.md` | not started | S0 |
| S2 | `internal/ws` + `internal/lock` + `cmd/redlite` (single leader serving clients) | `specs/s2_server.md` | not started | S1 |
| S3 | `reader/` (the embeddable client, K1/K2/K4 fixed) | `specs/s3_reader.md` | not started | S2 |
| S4 | `internal/replica` (heartbeat, full state, promotion) | `specs/s4_replica.md` | not started | S2 |
| S5 | `internal/persist` + `db/migration` (write-behind, hydration, cleaner) | `specs/s5_persist.md` | not started | S1 |
| S6 | `internal/api` (projects, API keys, export/import) | `specs/s6_api.md` | not started | S5 |
| S7 | `backendAdvo` migration: swap `advo_cache/reader` for this reader | (in advomedix-v2) | not started | S3, tag v0.1.0 |

Order: S1 → S2 → S3 → (S4 ∥ S5) → S6 → S7. A thin tracer bullet (open a sheet, set, get from a
second reader) ships at the end of S3 and is the first tag.

## Pending decisions (ask before S1)

- D-002 `sync.Map` vs `map`+`RWMutex` for the sheet store.
- D-003 Expiry: lazy-on-read + janitor interval.
- D-010 Protocol versioning: a `version` field on `open`, or a path (`/ws/v1`).
- D-011 WebSocket library: coder/websocket (ctx-native, concurrent-write-safe, zero deps; recommended) vs
  gorilla/websocket (advo_cache's choice; needs a write pump and manual ping/pong). `doc/reference/codeheim_patterns.md` § go_75.

## Reference knowledge

`doc/reference/README.md` indexes the checked outside sources; `.claude/CLAUDE.md` → "Reference sources"
says how to use and cite them. Distilled so far: Anthony GG's distributed cache stream, 46 code-heim repos.
Pending: `lru_cache.md` (Build Your Own Cache transcript), `advo_cache.md` (file:line walkthrough of the predecessor).
