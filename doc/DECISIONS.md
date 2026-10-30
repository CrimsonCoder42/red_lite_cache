# Decisions

One entry per decision that was actually made — storage, eviction, batching, failure behavior,
wire shape, auth, TTL policy, or anything else that was not obvious. Newest at the bottom.
Decisions made without being decided belong in `ASSUMPTIONS.md`, not here.

Format:

```
## D-001 <title> (YYYY-MM-DD, accepted)

**Context**
What forced a choice.

**Chosen**
What we do, specifically.

**Alternatives + why lost**
- <alternative> — why it lost

**Consequences**
What this commits us to, and what it costs.
```

---

## D-000 Rebuild, not fork (2026-10-29, accepted)

**Context**
`advo_cache` is a working service with eight known defects (SCOPE.md K1–K8), `go 1.21`, Viper, a
hand-rolled logger, and `sync.Map` everywhere. It lives inside the advomedix monorepo and is not a
pinnable dependency.

**Chosen**
A new module at `/Users/devinanderson/Desktop/red_lite_cache`, Go 1.27, built feature by feature
from the advo_cache reference with each defect fixed by design. The wire protocol stays
v1-compatible so a reader swap in `backendAdvo` is the only consumer change.

**Alternatives + why lost**
- Fork and patch advo_cache in place — carries every defect's shape forward; keeps it unpinnable.
- Adopt Redis — a second runtime to operate on the NUC and the DO box; loses the embedded O(1) reader.

**Consequences**
Everything is rebuilt and must be re-tested. The reference is read-only. Pending: D-001 to D-010 in SCOPE.md.

## D-001 Module path and repository (2026-10-29, accepted)

**Context**
Every import path and the consumer pin in backendAdvo depend on it.

**Chosen**
`github.com/CrimsonCoder42/red_lite_cache`, public repo, default branch `main`. First tag `v0.0.1` (S0 scaffold, no behavior).

**Alternatives + why lost**
- Private repo: needs GOPRIVATE + credentials on every machine and CI that builds backendAdvo. Raw transcripts in `examples/` are kept out of git instead.

**Consequences**
Anything in git is public. `examples/*.md` stays untracked; never commit secrets or transcripts.

## D-002 Sheet store: plain map + RWMutex (2026-10-29, accepted)

**Context** advo_cache used `sync.Map` per sheet (`cache/cache.go:19-20`); a TTL cache overwrites and deletes, so neither `sync.Map` case applies.
**Chosen** `map[string]entry` + `sync.RWMutex` per sheet. No sharding until a benchmark in `doc/` shows contention.
**Alternatives + why lost** `sync.Map` (advo_cache, code-heim go_11) — no len, no typed iteration, worse for overwrite-heavy keys ([sync.Map doc](https://pkg.go.dev/sync#Map)). Sharded map — premature; golang/go#17973 contention is unmeasured here.
**Consequences** One lock per sheet; a hot sheet is bounded by one RWMutex. `make bench` is the evidence before sharding.

## D-003 Expiry: lazy on read + one janitor, expiry broadcast (2026-10-29, accepted)

**Context** K5: one sleeping goroutine per `Set` (`cache/cache.go:44-51`); a re-set does not cancel the old timer. K3/R5: expiry never reaches readers.
**Chosen** `expiresAt` in the entry, checked on Get/Has; ONE janitor goroutine per process sweeps every sheet every `REDLITE_CACHE_JANITOR_INTERVAL`; TTL duration 0 = no expiry. Expired keys are broadcast as one `invalidate{keys:[…]}` per sheet per pass (and `replicate_expire` to the replica). The janitor runs only while the node is leader.
**Alternatives + why lost** Goroutine/timer per entry — unbounded, uncancellable. Timing wheel — better at 10⁷ keys, unneeded at this scale.
**Consequences** A reader sees an expiry broadcast up to one janitor interval late, but never serves it late locally: the reader holds the remaining TTL too (D-008).

## D-004 Reader reconnects forever (2026-10-29, accepted)

**Context** K4/R1: advo_cache gives up after `MaxReconnectAttempts` per server (`reader/reader.go:530-584`).
**Chosen** Reconnect forever, alternating leader/backup, capped exponential backoff with full jitter (cap 30 s, the consumer's G-01-15).
**Alternatives + why lost** Bounded attempts then error — the consumer has no recovery path but a restart.
**Consequences** `IsConnected` is the only signal; the consumer's `/health` recovers on its own when the link returns.

## D-005 Reader queues deletes while disconnected and replays them first (2026-10-29, accepted)

**Context** K2/R2: `Delete*` return `ErrNotConnected` before touching anything (`reader.go:242,266,295`).
**Chosen** While disconnected, `Delete/DeletePrefix/DeleteSuffix` apply locally, queue (bounded) and return nil. On reconnect the queue is replayed and acknowledged **before** the reader is marked connected, so no read is served from a map the replay has not cleared. Overflow collapses the queue to one whole-sheet flush (`delete_prefix ""`).
**Alternatives + why lost** Return `ErrNotConnected` (v1) — moves correctness into every consumer (the DROPPED SWEEPS queue). Sequence-based resync — needs a server change log longer than any outage.
**Consequences** A long outage plus overflow costs one full-sheet flush (DB reads), never a stale read.

## D-006 Config: stdlib env, read once, passed in (2026-10-29, accepted)

**Context** advo_cache used Viper (~20 transitive deps) and a global `config.AppConfig`.
**Chosen** `internal/config`: `.env` then environment, typed, validated, one `Config` value passed down. `os.Getenv` anywhere else is rejected by the hook.
**Alternatives + why lost** Viper — dependency weight in a module consumers import.

## D-007 Logger: log/slog with ReplaceAttr masking (2026-10-29, accepted)

**Context** advo_cache's hand-rolled ANSI logger and 150-line banner (K8).
**Chosen** `internal/logger` wraps slog; `ReplaceAttr` masks password, password_hash, value, key_hash, api_key, database_url; one banner line.
**Alternatives + why lost** Keep the custom logger — no structure, no masking.

## D-008 Reader local map honours TTL and is bounded (2026-10-29, accepted)

**Context** K1/R4: bare `sync.Map`, no expiry, no size cap (`reader.go:107,157`).
**Chosen** The server sends the remaining lifetime (`expires_in_ms`, relative so clocks need not agree) on every set reply, `replicate` and `sheet_state`; the reader stores it and misses after it. `Config.MaxEntries` (default 100 000) bounds the map; a new key on a full map evicts an expired entry, else an arbitrary one (a miss, never a stale read).
**Alternatives + why lost** LRU — a write lock on every Get hit. Absolute `expires_at` — wrong under clock skew between the two hosts.

## D-009 Server expiry is broadcast (2026-10-29, accepted)

See D-003; recorded separately because SCOPE K3 cites it.

## D-010 Protocol version on open (2026-10-29, accepted)

**Chosen** `version` (int, omitempty, absent = 1) on `open`; the open reply carries `protocol_version`. Clients stay on `/ws`; replicas use `/replica`. `protocol.Version` stays 1 while every change is additive.
**Alternatives + why lost** Path versioning (`/ws/v1`) — every v1 reader already dials `/ws`; a path change breaks them for no gain.

## D-011 WebSocket library: coder/websocket (2026-10-29, accepted)

**Chosen** `github.com/coder/websocket` v1.8.15 for server, replica and reader.
**Alternatives + why lost** gorilla/websocket (advo_cache) — one concurrent writer per conn, so we would own a write pump and ping/pong; coder serialises writes, takes a ctx on every call, has zero dependencies (it is the reader's only one).

## D-012 Promotion fencing: epoch in-protocol (2026-10-29, accepted)

**Context** K11: the replica promotes and the old leader never steps down (`replica/client.go:243-294`, no epoch anywhere).
**Chosen** A `uint64` epoch owned by the leader, persisted (`cluster_state`) when persistence is on, incremented by a replica on promotion. Carried by every replicate frame, heartbeat, `open` reply and reader write. A leader that sees a higher epoch (a replica registering, a reader write, or its peer's `/health` via `REDLITE_PEER_ADDRESS`) logs Warn and steps down to replica of the peer. A reader told a lower epoch than it has seen refuses that server and fails over. A replica refuses client `open` (`not leader`) until promoted.
**Alternatives + why lost** Postgres advisory lock deciding the leader — puts the database on the decision path even when persistence is off; moved to `doc/TODO.md`.
**Consequences** Writes the old leader accepted during a partition are discarded when it steps down (it reloads the new leader's snapshot). For a cache that is a miss, not data loss.

## D-013 Sheet lifecycle: registry-owned (2026-10-29, accepted)

**Context** K13 (`ws/handler.go:160-164` deletes the sheet on last disconnect), K14 (any client creates any sheet), K12 (promoted replica has no password).
**Chosen** A sheet is never deleted because its last collaborator left. `REDLITE_SHEET_AUTOCREATE=true` (v1 default) lets the first `open` create it with Warn `Sheet: autocreated`, owned by the `default` project; `false` means only the admin API (S6) creates sheets and `open` of an unknown sheet answers `sheet not found`. Password hashes are part of replicated and persisted state.
**Alternatives + why lost** Keep last-one-out deletion — persistence would hydrate a sheet the first disconnect then wipes.

## D-014 Ordering and backpressure: per-sheet mutex + bounded send queues (2026-10-29, accepted)

**Context** K9 (concurrent socket writes), K10 (no ordering), K15 (a slow collaborator stalls the sheet).
**Chosen** One `sync.Mutex` per sheet around apply + enqueue-to-every-collaborator (including the sender's reply). Each connection has a bounded send queue (`REDLITE_CONN_SEND_QUEUE`, 256) drained by its own writer goroutine; a full queue closes that connection (it resyncs on reconnect). No goroutine per sheet.
**Alternatives + why lost** Confinement goroutine per sheet (code-heim go_20) — a goroutine per tenant and a channel hop per write for the same guarantee.

## D-015 Persistence stack: pgx v5 + sqlc + golang-migrate (2026-10-29, accepted)

**Chosen** pgx v5.11 + pgxpool; sqlc v2 into `db/sqlc/{registry,entries,cluster}` (pgx/v5, json tags, interface, empty slices, uuid/timestamptz/jsonb overrides as the consumer's sqlc.yaml); golang-migrate v4.20.1, iofs source (migrations embedded), pgx v5 database driver, applied at boot when `REDLITE_PERSIST_ENABLED=true`. `db.Store` wraps the pool and the queriers with `ExecTx`, the consumer's `db/sqlc/store.go` shape.
**Alternatives + why lost** database/sql + lib/pq — no batch API, a second driver; GORM — reflection and a second schema source.

## D-016 Reader values: json.RawMessage, decode on Get (2026-10-29, accepted)

**Context** R8: values arrive as `map[string]any`; the consumer marshals twice per hit for type and isolation (`store.go:69-77`).
**Chosen** The reader stores the frame's `json.RawMessage`; `Get(key, target any) bool` decodes once into the caller's type (an isolated copy by construction); `GetRaw(key) (any, bool)` keeps the v1 shape. The other eight methods keep their signatures; `Get` is the one change, documented in `doc/release.md` for S7.
**Alternatives + why lost** Generic `Get[T]` — methods cannot be generic. Keep `Get(key) (any, bool)` — keeps the double pass in the consumer.

## D-017 Testing stack: stdlib testing (2026-10-29, accepted)

**Chosen** stdlib `testing`, no testify. `testing/integration/harness*.go`: `StartLeader(t)` / `StartReplica(t, leader)` on `127.0.0.1:0` through the same `app.Run` path `cmd/redlite` uses, a unique sheet per test, `t.Cleanup` stops everything. Persist tests read `REDLITE_TEST_DATABASE_URL`, use a per-run schema, and skip with `SKIPPED: persist (no REDLITE_TEST_DATABASE_URL)`.
**Alternatives + why lost** testify — a dependency for assertions the stdlib covers.

## D-018 Dev loop mirrors the consumer's (2026-10-29, accepted)

**Chosen** `.air.toml` (build `./cmd/redlite` into `./tmp/main`, exclude `_test.go`); Makefile with the consumer's target names (server, dev, test, sqlc, migrateup, migratedown, createdb, dropdb, postgres, network, db_reset, fresh_start) plus `dev-pair` (leader :8095 + replica :8096, two airs with separate binaries) and `bench`; `docker-compose.yml` with postgres + leader + replica.
**Alternatives + why lost** A bespoke script — a second set of names to remember across the two repos.

## D-019 Postgres role: control plane + durable copy, never the read/write path (2026-10-29, accepted — Devin, bootstrap.md)

**Context** `doc/persist/postgres_role.md` (the two "replace your stack with Postgres" transcripts, checked against the manual).
**Chosen** Postgres holds identity (projects, api_keys, sheets), cluster state (epoch), the audit log and a write-behind copy of sheet contents. No read, set, broadcast, expiry or lock touches it. No pg_cron, pgcrypto, RLS, GIN on value, or extensions beyond uuid-ossp.
**Alternatives + why lost** Postgres as the cache (UNLOGGED "poor man's Redis") — loses the O(1) in-process read the product exists for; PostgREST over the registry — leaks hash columns, a second process.
**Consequences** As built: `projects`, `api_keys`, `sheets`, `cluster_state`, `cache_entries` in 000001; sqlc packages registry, entries, cluster. **Not built:** `audit_events` + the `audit` package — the migration guard treats 000001 as shipped and only 000001 was pre-cleared, so it is the proposed pair `doc/bootstrap_2026-10-29/000002_audit.proposed.sql`, waiting for Devin.

## D-020 Secrets hashed in Go only (2026-10-29, accepted — Devin, bootstrap.md)

**Chosen** bcrypt for sheet passwords (`internal/sheet`), sha256 for API keys (`internal/api`); Postgres stores hashes and never receives plaintext. `cache_entries` is UNLOGGED unless `REDLITE_PERSIST_LOGGED=true` (then made LOGGED at boot), with the caveat that an UNLOGGED table is truncated on crash recovery and not streamed to a standby.
**Alternatives + why lost** pgcrypto `crypt()` — the plaintext crosses to the server and can land in `log_statement`/`pg_stat_statements`.

## D-021 Control bus: NOTIFY a reference, never data (2026-10-29, accepted — Devin, bootstrap.md)

**Chosen** After a registry commit the admin API runs `NOTIFY redlite_control, '<reference payload>'`; every instance LISTENs on one dedicated connection (`REDLITE_CONTROL_LISTEN`, persistence on only) and re-reads the row; the cleaner tick is the fallback (NOTIFY is not durable). Off when persistence is off.
**Alternatives + why lost** Polling the registry — a revoke takes a full poll interval and costs a query per tick per instance.

## D-022 Election mode: epoch (default) | advisory (2026-10-29, accepted — Devin, bootstrap.md)

**Chosen** `REDLITE_ELECTION=epoch` (D-012) by default; `advisory` makes `pg_try_advisory_lock` on a dedicated connection decide the leader (the session lock drops when that connection dies), and the epoch still fences. advisory requires persistence (config refuses otherwise).
**Alternatives + why lost** Advisory only — ties leader availability to Postgres even for a persistence-off deployment.

## D-023 R6 in the reader: oldest miss per key, held for the sweep-log window (2026-10-29, accepted — lead, per the bootstrap "decide and log" instruction)

**Context** Bootstrap wave 3 `TestRaceLoop` stress: two read-throughs of one key on one reader could still cache a stale value, because the first set consumed the shared miss record and the second fell back to the current seq (observed `server holds s:0:v = 1706, database is at 1707`). The consumer is exactly this shape: one reader, many request goroutines.
**Chosen** Per key the reader keeps the earliest miss of every `MissWindow/2` time slice, each live for `reader.Config.MissWindow` (default 30 s = `REDLITE_SWEEP_LOG_WINDOW`; at most 3 records per key). A set sends `base_seq` = the oldest live record and never consumes it; with no live record it sends the current applied seq. (A single oldest-record rule was tried first and failed the stress test 3/5: a miss just before the record expired went unrecorded. With slices, any read-through faster than `MissWindow/2` sends a base at or before its own miss.)
**Alternatives + why lost** A miss ticket passed back to the set (additive API) — every read-through call site in the consumer changes, which the CONTRACTS forbid before S7. Per-key outstanding-miss counts — a miss never followed by a set pins the key forever.
**Consequences** After a covering write, read-throughs of that key are refused (`ErrStale`, not cached) for up to 30 s. Never stale; at worst one DB read per request for that key during the window.

## D-024 Admission: optional api_key on open, policy by REDLITE_ADMISSION (2026-10-29, accepted — Devin, tenancy.md)

**Chosen** `open` gains `api_key` (additive, omitempty). `REDLITE_ADMISSION=password` (v1: sheet password only — the advomedix cut-over) or `key` (api_key required, must belong to the sheet's project, password still checked). Default `key` when `REDLITE_ENVIRONMENT=production`, else `password`. Every admission refusal is `{"success":false,"error":"not authorized"}` — never "wrong password" vs "wrong key" (no oracle). Code: `internal/tenant` (Directory.Admit), `internal/ws` open.
**Alternatives + why lost** Key-only everywhere — breaks the v1 reader backendAdvo runs today; a separate auth handshake frame — a second round trip and a new state in every client.

## D-025 Autocreate is development-only (2026-10-29, accepted — Devin, tenancy.md)

**Chosen** `REDLITE_SHEET_AUTOCREATE` defaults true only when `REDLITE_ENVIRONMENT=development`, and is forced off in any other environment even when set (`internal/config`). Autocreate never recreates a name a revoke dropped (`tenant.Directory.Revoked`); a suspended project's sheets still exist, so autocreate never applies to them.
**Alternatives + why lost** Keep the v1 default (on) — any client that reaches the port creates a tenant (K14).

## D-026 Sessions: in-memory per node, audited (2026-10-29, accepted — Devin, tenancy.md)

**Chosen** `internal/session.Registry` per node (map + RWMutex, session id = uuid), one entry per authenticated connection; last_seen and frame counters are atomics touched per inbound frame (no lock). No table for live sessions; open/close/kick rows go to `audit_events` (000002). GET /admin/sessions returns this node's sessions plus the peer's (fetched from the peer's /admin/sessions with the same admin key), each tagged `node`.
**Alternatives + why lost** A `sessions` table — per-node state that dies with the process would need cleanup on every crash; a write per open on the hot path.

## D-027 Suspend: reversible, data kept (2026-10-29, accepted — Devin, tenancy.md)

**Chosen** `projects.suspended_at` (000002). POST /admin/projects/{id}/suspend and /resume. Suspend closes every session of the project on every node (close 4001 `suspended`) and refuses `open`; sheets and entries stay in memory and Postgres and keep flushing. Propagated by NOTIFY `redlite_control` with persistence, else leader → replica as a `project_suspended`/`project_resumed` control frame.
**Alternatives + why lost** Only revoke — destroys data for what is often a billing or abuse pause.

## D-028 Keys: many per project, revoke one, rotate (2026-10-29, accepted — Devin, tenancy.md)

**Chosen** POST /admin/projects/{id}/keys (issue another, optional label), DELETE /admin/keys/{id} (revoke one; sessions admitted with it close 4002 `key revoked`), POST /admin/keys/{id}/rotate (issue + revoke in one tx, new key returned once). `api_keys.last_used_at` touched at most once a minute per key.
**Alternatives + why lost** One key per project — one leaked key takes the whole project down to rotate.

## D-029 Listing: read-only, admin-only, keyset paged (2026-10-29, accepted — Devin, tenancy.md)

**Chosen** GET /admin/projects, /admin/projects/{id}, /admin/sheets/{name}/sessions, /admin/sessions. `limit` (default 50, max 200) + opaque `cursor` (keyset on created_at,id), shapes in `protocol/admin.go`.
**Alternatives + why lost** Offset paging — skips/duplicates rows under concurrent inserts.

## D-030 Kick and terminal close codes (2026-10-29, accepted — Devin, tenancy.md)

**Chosen** DELETE /admin/sessions/{id} closes one connection with 4003 `kicked by admin`. The reader treats 4001/4002/4003 as terminal: no reconnect, Close() state, and `LastError()` reports ErrSuspended / ErrAdmitted (key revoked) / ErrKicked. 1000/1001/1006 and network errors keep reconnect-forever (R1).
**Alternatives + why lost** Kick with 1008 — the reader reconnects at once and the kick does nothing.

## D-031 Peer auth: REDLITE_CLUSTER_TOKEN (2026-10-29, accepted — Devin, tenancy.md)

**Chosen** /replica and the peer /health probe require `Authorization: Bearer <REDLITE_CLUSTER_TOKEN>` (constant-time compare). Missing or wrong → 401 before upgrade, nothing logged beyond remote addr and reason, no snapshot. Empty token in production refuses to start (config error); empty in development logs one Warn. Readers never use it. The admin key is unchanged; rotating it is an env change and a restart (`doc/release.md`).
**Alternatives + why lost** mTLS — certificate distribution for a two-node pair already on Tailscale; reuse the admin key — one leaked secret would open both the API and the replication stream.

## D-032 Consumer conformance: a miniature of backendAdvo's cache layer (2026-10-29, accepted — Devin, conformance request)

**Chosen** `testing/conformance/` (line 1 of every file `// Covers: backendAdvo cache layer`): `consumerstore.go` mirrors `cachedOne`/`cachedList` (`backendAdvo/cache/shared.go:109-157`), `invalidateDomain` WITHOUT the second sweep (`store.go:269-275`), `invalidateDomainBoth` (`store.go:293-298`), `domainPatientPrefix`/`domainCursorKey`/`domainFeedWindowKey` (`keys.go:60,100,127`) and five TTL constants, over the public reader API only; `FakeDB` counts loads.
**Alternatives + why lost** Run backendAdvo's own harness against red_lite_cache — it needs advomedix's database and its full app; a miniature isolates the cache contract and runs under `-race` here.

## D-033 Key corpus read from keys.go (2026-10-29, accepted)

**Chosen** `testing/conformance/keycorpus.go`: every distinct key SHAPE (≥40, one sample each, citing the keys.go builder lines), feeding the sweep tests and the R7 benchmark.
**Alternatives + why lost** Synthetic keys — the bootstrap's 50-equal-roots benchmark was a best case (audit finding R7).

## D-034 Three readers = three instances, both admission modes (2026-10-29, accepted)

**Chosen** Every conformance test runs 3 readers on one sheet (backendAdvo's three API instances share one sheet), in `REDLITE_ADMISSION=password` + autocreate (cut-over) and again in key mode with a registered project (target).

## D-035 reader/v1compat: the exact advo_cache reader surface (2026-10-29, accepted)

**Chosen** A new public package over `reader.Reader` with advo_cache's surface (`advo_cache/reader/reader.go`): `New(Config{LeaderAddr, BackupAddr, Sheet, Password, MaxReconnectAttempts (ignored), ReconnectDelay})`, `Get(key) (any, bool)` (a fresh decode per call), `GetString`, `GetMap`, `Has`, `IsNull`, `Set`, `SetWithTTL`, `Delete*`, `Close`, `IsConnected`, `CurrentServer`, `CacheSize`, and its three errors. Cut-over step 1 is an import-path change only.
**Alternatives + why lost** Change backendAdvo's call sites in one step — 464 read-throughs and the workaround deletions in one diff, no clean rollback.

## D-036 Migration plan from test evidence (2026-10-29, accepted)

**Chosen** `doc/consumer/backendadvo_migration.md`: each backendAdvo workaround mapped to the conformance test that retires it (or "still needed"), then the ordered cut-over (v1compat import swap → delete workarounds one at a time → `Get(key,&T)` direct), env mapping and a rollback per step.

## D-037 R7 index: decided by benchmark on real key shapes (2026-10-29, accepted — outcome recorded below when B1 runs)

**Chosen** Implement a two-segment index (`<root>:patient:<uuid>` head) in `internal/cache`; keep it only if benchstat (count=6) on corpus-shaped keys shows it wins, else remove it and record the numbers.
**Outcome — KEPT** (`internal/cache/store.go`; B1, count=6, `doc/conformance_2026-10-29/bench_b1.txt`; benchstat not on PATH, mean ± half-range by hand). 10 000 keys, 50 profiles × 8 roots, 25 keys per head: profile sweep 30 361 ns ±41% → **10 600 ns ±17% (0.35×)**; sweep of an absent profile 15 092 → **482 ns (0.032×)**; cost: a new key's set+delete 333 → 433 ns (1.30×). The sweep is the consumer's correctness path on every write; the extra ~100 ns is paid once per new key. `cache.NewWithoutHeadIndex()` stays exported as the comparison hook.
**On REAL shapes** (10 526 `keycorpus.go` keys, 8 roots × 50 profiles, swept root `med`; count=6, tight spreads — an earlier lead run with 9–35 µs spreads was throttled and is superseded; the checker reproduced these): profile sweep 18 626 → **8 276 ns (0.44×)**; sweep of a profile with nothing cached in that root (every `invalidateDomainBoth` requester, every write to a domain nobody read) 11 018 → **435 ns (0.039×)**; **cost: a new key's set+delete 320 → 457 ns (1.43×)**. Kept: the sweep runs on every write and is the correctness path; the cost is paid once per new key.

## D-038 Leak suite: testing/leaks, five named steps (2026-10-29, accepted — Devin, leaks request)

**Chosen** `testing/leaks/` (line 1 of every file `// Covers: memory and goroutine bounds`): one file per layer (`store_test.go`, `conn_test.go`, `reader_test.go`) + `e2e_test.go`, helpers in `leaks.go`. Every test logs BASELINE (after the harness is up, two settle passes) → FILL → OBSERVE (peak must exceed baseline, `leaks.Above`) → DRAIN (the real event: expire, delete, close, kick, revoke) → ASSERT (baseline ± a tolerance stated with its reason). Goroutines: `runtime.NumGoroutine` with a settle loop (20 × 10 ms, two equal samples); memory: `HeapAlloc` after two `runtime.GC` (never `Sys`).
**Alternatives + why lost** `go.uber.org/goleak` — a new module (dependency checkpoint not cleared) and it checks only goroutines, not maps, buckets or queues; heap-profile diffs — not assertable in a test.

## D-039 Clock: an injectable test seam, not a feature (2026-10-29, accepted)

**Chosen** `config.Config.Clock func() time.Time` (never read from env; nil = `time.Now`) → `sheet.Options.Now` (already existed) and `lock.NewManager(now)`; `cache.Store` needs none (every time-dependent call takes `now`). The reader gets an unexported clock field only: exporting it through `reader.Config` would be new public surface, and L2–L4 need none (expiry reaches readers as `invalidate{keys}`). Harness: `integration.NewClock`, `WithClock`. The janitor and lock cleaner keep ticking on real time; they compare against the clock.
**Alternatives + why lost** Very short real TTLs everywhere — sleeps, flaky under `-race`; a global clock variable — shared mutable state across parallel tests.

## D-040 Measurable sizes: Stats() counts only (2026-10-29, accepted)

**Chosen** `Stats()` returning counts (never keys or values) on `cache.Store`, `sheet.Registry`, `reader.Reader` (exported `reader.Stats`, diagnostic, not part of the compatibility contract), `lock.Manager`, `tenant.Directory`, `persist.Writer`, `ws`; `/health` carries them under `"stats"` only when `REDLITE_ENVIRONMENT=development` (additive, omitted otherwise). The leak tests assert on these.
**Alternatives + why lost** Reflection / unsafe size probes from tests — breaks on every refactor and reads unexported state; expvar — a global registry, and exposed in production.

## D-041 Bound the tenant sets (2026-10-29, accepted)

**Chosen** Revoked sheet names and revoked key ids in `tenant.Directory` are capped at `REDLITE_TENANT_MEMORY_MAX` (default 10 000 each), oldest dropped, one Warn on the first drop, counted in Stats; and cleared when the tenancy state is reloaded from Postgres (persist on), because Postgres is then the truth.
**Alternatives + why lost** Leave them unbounded — every revoke since boot stays in memory forever; a TTL per entry — a second expiry loop for two sets that are rarely written.

## D-042 Sheets are never evicted from memory (2026-10-29, accepted — D-013 stands)

**Chosen** The leak tests assert the sheet count equals the sheets the test created; an emptied sheet (0 collaborators, 0 entries) costs only its struct: its Stats are 0/0 with no stale index buckets or sweep-log entries kept beyond their bound.
**Alternatives + why lost** Evict idle sheets — a sheet is a tenant's registration (D-013, K13), not a cache entry; evicting it would make the next open fail or autocreate.
