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
