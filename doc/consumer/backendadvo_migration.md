# backendAdvo → red_lite_cache migration plan (S7)

_Written 2026-10-29 from test evidence (D-036). backendAdvo = `advomedix-v2/backendAdvo` (read-only from
here; every line cited was read). Every behaviour claim names a conformance test in
`testing/conformance/` (a miniature of backendAdvo's cache layer, D-032) that ran green under `-race`:
`bash testing/run.sh` → `327 | 314 | 0 | 13` (13 = persist, `SKIPPED: persist (no REDLITE_TEST_DATABASE_URL)`);
conformance alone `60 | 60 | 0 | 0`; every C test runs 3 readers on one sheet (= the three API
instances) in **both** admission modes (`/password` = cut-over, `/key_C13` = target)._

## 1. Workaround → evidence

| backendAdvo workaround | Lines | Conformance evidence | Verdict |
|---|---|---|---|
| Banner 1: deletes dropped while disconnected → DROPPED SWEEPS queue (`droppedSweepQueue`, `record`, `collapseToRoot`), replayed in `linkReady` | `cache/store.go:18-35`, `:340-520` (`:408` queue, `:447` linkReady, `:473` replay) | `TestC8DisconnectRetiresDroppedSweeps` — instance 2 behind a cut link sweeps while down (returns nil), reconnects, and its first read misses and reloads; no stale read on any instance; the consumer `Store` has one field (no queue) | **delete** (step 2a) |
| `linkReady()` gate: reads miss while disconnected | `cache/store.go:447-470`, called from `Get` `:166-170` | `TestC8…` (reads miss while down), `TestC9BootWhileCacheDown`; reader R3 (`reader` `TestR3DisconnectedReadsMiss`) | **delete** (step 2a) |
| Banner 1b / G-01-16: `invalidateDomain` sweeps a second time `secondSweepDelay` (2 s) later | `cache/store.go:36-39`, `:269-275`, `:388` | `TestC2WriteInvalidatesEverywhere` — ONE sweep, readers 2 and 3 reload the new row; `TestC3ReadThroughRaceNeverRecaches` — 200 iterations per mode of a read-through parked mid-load of the OLD row while another instance writes + sweeps: its Set is refused (`ErrStale`), nobody reads the old value | **delete** (step 2b) |
| Banner 2: local map has no TTL / no bound | `cache/store.go:41-49` | `TestC7TTLBounds` — a short-TTL entry expires on all three readers and the DB loads again; a TTL-0 entry does not expire; reader `TestR4ExpiryAndBound` (MaxEntries) | comment only — no code to delete |
| Banner 3: server expiry not broadcast — "never rely on a TTL for correctness" | `cache/store.go:51-60` | `TestC7TTLBounds` (every reader expires); invalidation remains the correctness path (`TestC2`) | keep the rule, delete the caveat |
| Banner 4: `DeletePrefix` scans the whole local map — "sweep once per write" | `cache/store.go:62-67` | `TestC4PrefixSweepScope` (93 real shapes × 3 profiles: exactly p1's keys of the root go); B1 on 10 526 corpus keys: server profile sweep 18 626 → 8 276 ns (0.44×), sweep of an absent profile 11 018 → 435 ns (0.039×), at 1.43× the cost per new key (D-037). The **reader's** local DeletePrefix still scans its map under one lock (reader/local.go) | keep "once per write" (still good practice); delete the full-scan warning for the server |
| Banner 5 / `Store.Get` double JSON pass (marshal + unmarshal per hit, for type and isolation) | `cache/store.go:69-77`, `:166-178` | reader `TestR8TypedIsolatedGet`; every C test reads through `CachedOne[T]` / `CachedList[T]` with the typed `Get(key,&T)`; `TestEdgeUndecodableValueIsMiss` (a value of the wrong shape is a miss, the loader runs) | **delete** (step 3) |
| Boot `Logger.Fatal("cannot connect to cache")` | `cmd/api/main.go:112-120` | `TestC9BootWhileCacheDown` — `New` with the leader down returns a usable reader, reads go to the DB, it connects when the leader appears; reader `TestR10NewWhileDown` | **delete** (step 2c) — keep Fatal only for `ErrAuthFailed` |
| `/health` 503 while disconnected | `api/routes/router.go:129` | reconnect-forever: reader `TestR1ReconnectForever`, integration `TestReconnectForever` | **keep** (a load balancer should still drain a disconnected instance; it now recovers by itself) |
| Errors never cached; zero values cached | `cache/shared.go:146-157` | `TestC10ErrorsNeverCached`, `TestEdgeZeroValueCached` | unchanged behaviour |
| Cursor pages keyed by (nanos, id) | `cache/keys.go:81-102` | `TestC11CursorKeysDistinct` — two pages, two keys, one sweep clears both | unchanged |
| `DeleteSuffix(userPermissionsSuffix)` | `cache/permissions.go:132`, `cache/roles.go:94` | `TestC5SuffixSweep` — `…:<tenant>:permissions` × 3 tenants removed, `…:profile` kept | unchanged |
| `invalidateDomainBoth` (owner + requester, `uuid.Nil` skipped) | `cache/store.go:293-298` | `TestC6BothProfiles` | unchanged |

**Expected, explained:** each instance misses once per key on its first read and loads it itself
(`TestC1ReadThroughCachesOnce`: one load per instance, zero after) — a set through one instance reaches
the others as `replicate`, but a *miss* is local. After a sweep, a read-through that raced it is refused,
so the first round after a write can cost one extra DB load (`TestC2` logs 4–5 loads for 6 reads depending on the run, then 0):
the price of dropping the second sweep, never a stale read.

## 2. Configuration mapping

| backendAdvo (`util/config.go:19-24`) | red_lite_cache |
|---|---|
| `CACHE_LEADER_ADDR` | `reader.Config.LeaderAddr` (`ws://<leader>:8095/ws`) |
| `CACHE_BACKUP_ADDR` | `reader.Config.BackupAddr` (`ws://<replica>:8096/ws`) |
| `CACHE_SHEET` | `reader.Config.Sheet` |
| `CACHE_PASSWORD` | `reader.Config.Password` |
| — (step 4, optional) | `reader.Config.APIKey` once the cache runs `REDLITE_ADMISSION=key` |

On the **cache nodes**: `REDLITE_ENVIRONMENT=production`, `REDLITE_ADMISSION=password` (the v1 open
backendAdvo sends today; `TestC*/password`), `REDLITE_SHEET_AUTOCREATE` is forced off in production —
so **create the sheet first** (`doc/release.md` runbook: register a project, create `patient-access`
with the existing password), the same `REDLITE_CLUSTER_TOKEN` on leader and replica, `REDLITE_ADMIN_KEY`,
`REDLITE_PEER_ADDRESS` on both. Ports 8095/8096 (advo_cache keeps 8081/8082 during the cut-over).

## 3. Cut-over, in order — each step ships and rolls back alone

backendAdvo's own harness on disk is `curl_tester/run.sh` (phases `phase-0-signup.sh` … `phase-11-account-billing.sh`;
the cache-heavy ones are `phase-4-same-tenant-infant-care-page.sh`, `phase-5-same-tenant-vitals-detail.sh`,
`phase-6-cross-tenant-caregiver-crud.sh`, `phase-9-cross-tenant-infant-care.sh`) and `curl_tests/*.sh`
(e.g. `infant-care-test.sh`, `patient-profile-test.sh`, `cross-tenant-permission-test.sh`). The request's
`testing/new_backend/helpers.sh` (`assert_write_invalidates`), `vitals/blood_pressure.sh` and
`infant/diaper.sh` are **not on disk** under backendAdvo — if they live elsewhere, run them at each step too.

**Step 1 — import swap, zero behaviour change.** Tag red_lite_cache v0.3.0. In backendAdvo: replace
`github.com/CrimsonCoder42/advo_cache/reader` with `github.com/CrimsonCoder42/red_lite_cache/reader/v1compat`
in `cache/store.go` (the only import site), `go get …/red_lite_cache@v0.3.0`, drop the `replace … => ../advo_cache`
and `COPY advo_cache/` (Dockerfile), `go mod tidy && go mod vendor` (backendAdvo vendors). Point
`CACHE_*_ADDR` at the red_lite_cache pair. v1compat is advo_cache's surface (`TestC12V1CompatSurface`):
`Get(key) (any,bool)` still returns `map[string]any`, so `Store.Get`'s double pass keeps working.
*Gate:* `curl_tester/run.sh` + `curl_tests` green. *Rollback:* revert the import and the address env.
Behaviour differences already active in step 1 (all safer, `reader/v1compat` doc): deletes while down are
queued and return nil; reads miss while down; reconnect never gives up; `New` no longer fails when the
cache is down; a stale read-through `Set` returns nil and is not cached.

**Step 2 — delete the workarounds, one per deploy.**
- 2a `droppedSweepQueue` + `linkReady` + `replayDroppedSweeps` (`store.go:340-520`, the gate in `Get` `:167-169`, the queue call in `sweepPrefix` `:334-345`). Evidence `TestC8`, `TestC9`. Gate: `phase-5`, `phase-9`, an outage drill (stop the leader, write, restart, read).
- 2b `secondSweepDelay` and the `time.AfterFunc` in `invalidateDomain` (`store.go:269-275`, `:388`). Evidence `TestC2`, `TestC3`. Gate: `phase-4`, `phase-6` (write-then-read invalidation).
- 2c `Logger.Fatal("cannot connect to cache")` → Fatal only on `ErrAuthFailed`, Warn otherwise (`cmd/api/main.go:118-120`). Evidence `TestC9`. Gate: boot the API with the cache down; it serves from the DB.
- Banner comments 1–5 (`store.go:8-87`) rewritten to what is now true.
*Rollback:* each is one revert.

**Step 3 — typed Get, drop v1compat.** Import `…/red_lite_cache/reader`, `Store.Get` becomes
`return s.reader.Get(key, target)` (deletes the double pass `store.go:170-177`); `IsConnected`, `Set*`,
`Delete*`, `Close` are unchanged signatures. Evidence `TestR8TypedIsolatedGet`, every `TestC*` (they use
`Get(key,&T)`). *Gate:* full harness. *Rollback:* back to v1compat (step 1 state).

**Step 4 (optional, later) — key admission.** Add `reader.Config.APIKey` from a new `CACHE_API_KEY`, then
switch the cache to `REDLITE_ADMISSION=key`. Evidence `TestC*/key_C13` (C1–C6 green in key mode). *Rollback:*
`REDLITE_ADMISSION=password`.

## 4. Still needed after the cut-over

- `/health` 503 while disconnected (`router.go:129`) — operational, not a workaround.
- "Sweep once per write" — the reader's local prefix delete still walks its map (reader/local.go).
- Read-through refused after a sweep for up to `MissWindow` (30 s, D-023): a hot key written often is
  cached less often than before — measure in production; never stale.
- Not proven here: the per-entry minute TTLs backendAdvo passes (`TTLAnalytics=5` …) expiring on the
  readers. `TestC7TTLBounds` shortens the server default (300 ms) because the wire TTL unit is minutes;
  the per-entry path is the same code (`expires_in_ms` on the ack), but no test waits five minutes.
