# Purpose — why red_lite_cache exists

_Researched 2026-10-29 from the one real consumer: `advomedix-v2/backendAdvo` (`cache/store.go`,
`cmd/api/main.go:100-135`, 79 files under `cache/`, `.gitea/workflows/deploy-*.yml`). Every claim cites
file:line in that repo. Re-verify before relying on a line; the consumer keeps moving._

## The job, in one sentence

Give every backendAdvo API instance an in-process, O(1) read copy of the same cache, and make a write
or invalidation on one instance reach every other instance **before it can serve a stale read** — with
no stale read ever surviving a disconnect.

## Who uses it, and how

| Fact | Evidence |
|---|---|
| One consumer today: backendAdvo, through one wrapper, `cache.Store` | `backendAdvo/cache/store.go:99-104` |
| Pinned by a local replace, not a tag | `backendAdvo/go.mod:6,86` (`replace … => ../advo_cache`) |
| 3 production API instances, all on **one sheet** `patient-access` | `.gitea/workflows/deploy-backendadvo-{production,instance2,api2-instance3}.yml` (`CACHE_SHEET`, 3 files) |
| Leader on the NUC, replica on DigitalOcean, over Tailscale | `deploy-backendadvo-production.yml:139-140`, `deploy-advocache-leader.yml:49-62` |
| Boot is fatal if the cache is unreachable | `cmd/api/main.go:118-120` (`Logger.Fatal("cannot connect to cache")`) |
| Switching `DB_SOURCE` serves the other database's cached rows; TODO per-environment isolation | `cmd/api/main.go:98-104` |

### The API surface actually called (79 files, `backendAdvo/cache/*.go`)

| Reader call | Call sites | Notes |
|---|---|---|
| `Get` | 124 | wrapped in `Store.Get`: JSON marshal + unmarshal per hit (`store.go:166-178`) |
| `SetWithTTL` | 126 | TTL in **minutes**; `ttl` field on the `set` frame (`advo_cache/reader/reader.go:213-237`) |
| `Delete` | 138 | exact record keys |
| `DeletePrefix` | 6 | the correctness mechanism: `invalidateDomain` sweeps `<root>:patient:<uuid>` (`backendAdvo/CLAUDE.md` "The cache layer") |
| `DeleteSuffix` | 3 | `user:<uuid>:<tenant>:permissions` shape |
| `Has`, `IsConnected`, `Close` | few | `IsConnected` drives `/health` 503 |
| `lock`/`unlock`, `set_null`/`IsNull`, `dump`, `register_replica` | **0** from backendAdvo | kept for wire compatibility; not product drivers |

`Set` with no TTL is unused; every write names a TTL constant from `keys.go` (`TTLAnalytics` ×55,
`TTLTenant` ×17, `TTLCampaign` ×17, …).

## What the consumer had to build around the reader — the requirements

`backendAdvo/cache/store.go:8-87` is a banner listing advo_cache's defects and the workaround for each.
**Each workaround is a requirement this project must meet inside the library**, so the consumer can delete
it. This table is the acceptance list for S3 (reader) and S7 (migration).

| # | Requirement | Defect today | Consumer workaround (file:line) | Slice |
|---|---|---|---|---|
| R1 | Reconnect forever, capped back-off | gives up after 3 tries per server (`reader.go:530-584`) | none; `/health` 503 until restart (`store.go:28-32`, decision G-01-15) | S3 |
| R2 | A delete issued while disconnected is **queued and replayed** before the first read after reconnect | `Delete*` return `ErrNotConnected` before touching the map (`reader.go:242,266,295`); reconnect reloads the server copy that never saw it | prefix sweeps queued + replayed in `store.go` "DROPPED SWEEPS"; exact `Delete` still drops (`store.go:33-34`) | S3 |
| R3 | While disconnected, reads miss | serves the stale local map | `linkReady()` gate in `Store.Get` (`store.go:446-460`) | S3 |
| R4 | The local copy honours TTL and has a size bound | bare `sync.Map`, no expiry, no eviction (`reader.go:107,157`) | none; open-ended key families grow without bound (`store.go:41-49`) | S1/S3 |
| R5 | Server-side expiry reaches every reader | TTL expiry not broadcast (`store.go:51-60`) | "never rely on a TTL for correctness" | S2/S3 |
| R6 | A write-then-read race cannot re-cache pre-write rows | read-through Set can land after the sweep | `invalidateDomain` sweeps **twice, 2 s apart** (`store.go:36-39`, G-01-16) | S3 (versioned sweeps) |
| R7 | `DeletePrefix` is not a full-keyspace scan per call | `sync.Map.Range` over every key (`reader.go:280`) | "sweep once per write, never per row" (`store.go:61-67`) | S1 |
| R8 | A hit returns the caller's type without a JSON round trip, and the caller cannot mutate another request's value | broadcast values arrive as `map[string]any`; consumer marshals twice per hit (`store.go:69-77`) | double JSON pass, kept because it also gives isolation | S3 |
| R9 | A write is acknowledged, or its failure is visible | `set` is fire-and-forget (`reader.go:228`) | none | S2/S3 |
| R10 | Boot does not die when the cache is down | `New` fails if the first connect fails | `Logger.Fatal` (`main.go:118-120`) | S3 |
| R11 | Sheets cannot collide across environments | one sheet name for every DB | TODO (`main.go:98-104`) | S2 (sheet naming / D-id) |
| R12 | Sheet credentials are not a shared literal | `CACHE_PASSWORD=secret` in the deploy workflow (`deploy-backendadvo-production.yml:142`) | none | S6 / advomedix (FOUND, NOT FIXED there) |

### Status — bootstrap 2026-10-29

`bash testing/run.sh` (-race). Integration tests run against a real leader (and replica) on a free port.

| # | Status | Test that fails on advo_cache and passes here | Consumer code it lets go (S7) |
|---|---|---|---|
| R1 | met | `TestReconnectForever`, `TestDisconnectLoop`; `reader` `TestR1ReconnectForever` | the G-01-15 note; `/health` recovers by itself |
| R2 | met (D-005) | `TestDisconnectLoop`; `reader` `TestR2DeletesQueuedAndReplayed`, `TestR2QueueOverflowCollapses` | DROPPED SWEEPS queue, `sweepPrefix` error path |
| R3 | met | `TestDisconnectLoop`; `reader` `TestR3DisconnectedReadsMiss` | `linkReady()` gate |
| R4 | met (D-008) | `TestKeyLoop`; `reader` `TestR4ExpiryAndBound`; leaks `TestL3ReaderBoundHoldsUnderChurn` (MaxEntries 1 000 held at every sample of 50 000 frames, heap +1.6 %) | the ADVO_CACHE §2 caveat |
| R5 | met (D-003) | `TestKeyLoop` (invalidate{keys}); `internal/sheet` `TestRunJanitor` | "never rely on a TTL" caveat (§3) |
| R6 | met (D-023) | `TestRaceLoop` (prefix, exact, suffix, two_misses_one_reader, stress) | the second sweep 2 s later (G-01-16) |
| R7 | met | `internal/cache` `BenchmarkDeletePrefixIndexed` 313 µs vs naive 1740 µs at 100k keys (`doc/bootstrap_2026-10-29/bench_s1.txt`) | "sweep once per write" caveat (§4) |
| R8 | met (D-016) | `reader` `TestR8TypedIsolatedGet` | the double JSON pass in `Store.Get` — **`Get(key, &target) bool` is the one signature change** |
| R9 | met | `reader` `TestR9AckedSet`; `TestRaceLoop` (ErrStale) | — |
| R10 | met | `TestNewWhileLeaderDown`; `reader` `TestR10NewWhileDown` | `Logger.Fatal("cannot connect to cache")` |
| R11 | met by the registry | `TestAutocreateOffRegistryPath` — one sheet per environment, created by the admin API | the `main.go:98-104` TODO |
| R12 | server side met | `TestAutocreateOffRegistryPath` (a project's sheet password + API key issued, not a literal) | the deploy workflow still carries `CACHE_PASSWORD=secret` — FOUND, NOT FIXED in advomedix-v2 |

## What success looks like

- backendAdvo swaps the import (`advo_cache/reader` → `red_lite_cache/reader`), pins a tag, and **deletes**
  the DROPPED SWEEPS queue, the `linkReady` gate, the second sweep and the double JSON pass — and its
  existing cache tests (`testing/new_backend/…` write-then-read invalidation asserts) stay green.
- The v1 wire format (`doc/protocol/wire.md`) still works against the old reader during the cut-over.
- Every R-row above has an integration test in `testing/integration/` that fails on advo_cache's
  behaviour and passes here.

### Status — consumer conformance 2026-10-29 (final, before S7)

The R rows above re-proven at the consumer level, by a miniature of backendAdvo's own cache layer
(`testing/conformance`, 3 readers = 3 instances, both admission modes, `60 | 60 | 0 | 0` under `-race`):
R1/R3/R10 `TestC9BootWhileCacheDown`, `TestC8…` · R2 `TestC8DisconnectRetiresDroppedSweeps` · R4/R5
`TestC7TTLBounds` · R6 `TestC2WriteInvalidatesEverywhere` (one sweep) + `TestC3ReadThroughRaceNeverRecaches`
(200 iterations) · R7 `TestC4PrefixSweepScope` (93 real key shapes) + B1 on 10 526 corpus keys (miss sweep
0.039×, profile sweep 0.44×, new key 1.43× — D-037) · R8 every `TestC*` through typed `Get` +
`TestEdgeUndecodableValueIsMiss` · R9 `TestC3` (the refused Set is visible as `ErrStale`). Which
backendAdvo lines each retires: `doc/consumer/backendadvo_migration.md`.

## Admission and removal — how a project other than advomedix joins (D-024…D-031)

- **Registration is admin-only.** `POST /admin/projects` with `REDLITE_ADMIN_KEY` creates a project
  and returns one API key, shown once (sha256 at rest). The admin (or the project, with its key) then
  creates its sheets. Nothing else creates a project; outside development nothing else creates a sheet
  (autocreate is forced off, D-025).
- **Admission.** In production (`REDLITE_ADMISSION=key`, the default there) `open` must carry the
  project's `api_key` *and* the sheet password; any mismatch is one answer, `not authorized`. advomedix
  cuts over with `REDLITE_ADMISSION=password` (v1 `open`, unchanged) until it sends a key.
- **Sessions.** Every authenticated connection is a session — project, sheet, key, remote, connected_at,
  last_seen, frames — listed by `GET /admin/sessions` across both nodes, audited on open/close/kick.
- **Removal, three levels, immediate on every node:** kill one session (close 4003); suspend a project
  (every session closed 4001, `open` refused, data kept, reversible with resume); revoke a project
  (keys revoked, sheets dropped). Per key: issue another, revoke one (its sessions close 4002), rotate.
  A reader treats 4001/4002/4003 as terminal and reports them through `LastError()`.
- **Peers.** `/replica` and the fencing `/health` body require `REDLITE_CLUSTER_TOKEN`; production does
  not start without it.

The operator runbook (exact curl lines) is in `doc/release.md`.

## Out of scope (v1)

Consensus/leader election (static leader + one replica, as today), general Redis features, non-Go
clients beyond the documented wire format, locks/`set_null`/`dump` improvements (kept, not grown).
