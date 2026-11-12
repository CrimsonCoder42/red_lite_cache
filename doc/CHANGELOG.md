# Changelog

- 2026-11-11 — **Fix: values over 1 MiB ended the session.** The server's default receive limit
  (`REDLITE_MAX_FRAME_BYTES`) was 1 MiB, so a 1.1 MiB set ended the session `RESOURCE_EXHAUSTED`
  ("received message larger than max (1176241 vs. 1048576)") and the reader reconnected. The default
  is now 4 MiB (`rpcwire.DefaultMaxMessageBytes`, 4 × the chunk). The reader refuses a bigger set with
  `ErrTooLarge` instead of sending it. Compose admission now defaults to `password` (the dev consumer
  is v1compat), and `make wipe` removes `.redlite/dev-project.env`. Tests: `TestEdgeLargeValue`,
  reader `TestGRPCValueOverChunk`.
<!-- grpc_build_2026-11-11 -->
- 2026-11-11 — **The wire is gRPC; JSON over WebSocket is removed** (`doc/grpc_build_2026-11-11/`, `doc/specs/s10_grpc.md`,
  D-048, D-049, D-050). Contract `proto/redlite/v1/{cache,replica}.proto` → `protocol/redlitev1` (`make proto`, staleness test);
  `protocol/rpcwire` converts at the edge. `Cache.Session`: one bidi stream per session, replies and broadcasts in one order,
  every message names its sheet; sheet states and replica snapshots in sorted ≤ 1 MiB chunks (S9 step 4; 100k keys tested).
  `Replica.Follow` / `Replica.Probe` with the cluster token as call credentials. Server core moved to `internal/rpc`
  (Conn, router, admission, Handler behind a `Transport`; abortable sends; stream status + `ErrorDetail` close codes);
  `internal/ws` → `_delete/`; `github.com/coder/websocket` dropped. Admin REST + `GET /health` on `REDLITE_ADMIN_PORT`
  (:8097; compose 8097/8098), `grpc.health.v1` on the client port, `REDLITE_PEER_ADMIN_ADDRESS`. Reader/v1compat APIs
  unchanged; addresses are `host:port` (`ws://` refused); reconnect replay windowed to 64 unacked deletes (fixes a
  replay ↔ slow-consumer loop). Every suite ran on both wires first (grpc `412 | 407 | 1 | 4`, ws `412 | 409 | 1 | 2`); after
  removal `404 | 401 | 1 | 2` (the failure is the pre-existing jsonb `TestPersistLoop`), smoke PASS. Benchmarks (medians of 6,
  WS → gRPC): acked set 84 → 121 µs, broadcast to 30 readers 271 → 506 µs, 100k-key connect 225 → 275 ms (bytes −16 %),
  stalled peer ended 5.1 s → 58 ms. New tests: `internal/rpc` (bufconn, in-memory transport), reader fake gRPC server,
  `testing/blackbox` (address + .proto only), `testing/wirebench`.

<!-- docker_ready_2026-11-11 -->
- 2026-11-11 — `docker compose up -d` (or `make up`) gives a ready cache (`doc/docker_ready_2026-11-11/`, D-047): postgres
  (127.0.0.1:5434, `redlite_test` created by `docker/postgres/10-create-test-db.sql`), the leader with persistence on and a
  `/health` healthcheck (127.0.0.1:8095), and a one-shot `bootstrap` (`docker/bootstrap/bootstrap.sh`, Dockerfile target
  `bootstrap`) that registers a development project and sheet through the admin API and writes
  `./.redlite/dev-project.env` (mode 600); the replica moved behind `--profile cluster` (127.0.0.1:8096). `.dockerignore`
  (build context 83.8 MB → 1.02 MB; `.env` never reaches a build stage); Dockerfile on 8095 with a healthcheck; Makefile
  `up`/`cluster`/`down`/`wipe`/`smoke`/`test-persist`; `testing/compose_smoke.sh` and the gated
  `testing/compose/compose_test.go` (`REDLITE_COMPOSE_TEST=1`). Harness with the persistence tests running:
  `389 | 386 | 1 | 2`, 0 DATA RACE (the one failure is pre-existing: `TestPersistLoop`, jsonb re-formats values — TODO).

<!-- ordered_index_build_2026-11-11 -->
- 2026-11-11 — S9 steps 1, 2, 3, 5 built (`doc/ordered_index_build_2026-11-11/`; D-044, D-045, D-046; wire and
  `reader/v1compat` unchanged). **Groups** (`internal/cache/groups.go`, `ordered.go`): forward + reversed B+trees,
  128 keys per node, merge below 32, shared by the server `Store` (`internal/cache/store.go`) and the reader's local
  map (`reader/local.go`); `internal/cache/index.go` (segment + `patient` head index) moved to `_delete/`. One-match
  prefix / suffix delete at 100 000 keys 2.31 / 1.86 µs (colon keys) and 1.93 / 2.23 µs (`/` keys), shape-independent
  (the old index: 1.0–1.2 ms on `/` keys); reader bytes per entry 338.9 → 270.4. **Wheel** (`internal/cache/wheel.go`):
  expiry filed by second, decided on the wall clock, compacted above 2 × live; the janitor's idle pass at 100 000 keys
  896.6 µs → 0.79 µs; the reader drops expired entries locally every second (`reader/reader.go` `expireLoop`).
  `cache.Stats` / `reader.Stats`: bucket counts → `ForwardNodes`, `ReverseNodes`, `WheelItems` (+ `WheelSlots` on the
  server); `/health` `root_buckets`/`tail_buckets` carry the node counts, `head_buckets` is 0. Harness before
  `369 | 354 | 0 | 15`, 0 DATA RACE; after: `doc/ordered_index_build_2026-11-11/out/harness_after.txt`.

<!-- reader_optimize_2026-11-10 -->
- 2026-11-10 — reader local map optimised (D-043; `doc/reader_optimize_2026-11-10/`; reader API, Config and wire
  unchanged; `reader.Stats` and the development-only `/health` `stats` gain fields). **Panics visible** (S1): a recovered
  panic is counted — `ws.ConnOptions.Panics`, `ws.Handler.Panics`, `protocol.HealthStats.Panics`, `reader.Stats.Panics` —
  and every server and reader the integration harness starts fails its test at cleanup when its count is above 0
  (`integration.CheckNoPanics`). **Reader index** (S2): `internal/cache/index.go` holds the first-segment, last-segment
  and patient-head index moved unchanged out of `store.go` (the server store embeds it; `store_test.go` untouched);
  the reader's local map uses it through `cache.Index`, so a one-match prefix / suffix delete at 100 000 entries is
  953 / 873 ns instead of 1.5 / 1.6 ms (1.30× / 1.31× its cost at 1 000); `reader.Stats` gains `RootBuckets`,
  `TailBuckets`, `HeadBuckets`. **Sampled LRU** (S3, D-043): slots `{key, access, expires}` in a dense array, 5 uniform
  draws per eviction, access stored atomically under the read lock at 100 ms resolution, mutex padded to its own cache
  line. Cost: 94.4 → 338.9 bytes per entry; put on a full map at 100 000 entries +43% vs HEAD. **Load measured** (S4,
  no default changed): 0 slow-consumer kicks at 50 / 500 / 5 000 writes/s × `REDLITE_CONN_SEND_QUEUE` 256 / 1 024 /
  4 096 (`testing/load`, `REDLITE_LOAD_TEST=1`). New tests: reader `audit_test.go`, `panics_test.go`, `index_test.go`,
  `eviction_test.go`; `internal/ws/panics_test.go`; `testing/integration/overflow_test.go`,
  `testing/leaks/reader_index_test.go` (L13), `testing/stress/`, `testing/load/`. `doc/reference/lru_cache.md`.
  `bash testing/run.sh`: `358 | 344 | 0 | 14` before; final `369 | 354 | 0 | 15` (14 = persist, no
  `REDLITE_TEST_DATABASE_URL`; 1 = load, no `REDLITE_LOAD_TEST`), after one run where
  `internal/persist` `TestRevokeDiscardsHeldBatch` (untouched) timed out once and passed on 3 re-runs.

- 2026-10-29 — leak suite (D-038…D-042; next tag **v0.3.1**: additive `reader.Stats`, no wire change beyond the
  development-only `/health` `"stats"`): `testing/leaks/` L1–L12 + 6 edge cases (BASELINE → FILL → OBSERVE → DRAIN →
  ASSERT); harness `integration.NewClock/WithClock/GoroutineBaseline/WaitGoroutines/HeapAlloc`; `config.Clock`
  (test seam) and `REDLITE_TENANT_MEMORY_MAX`; `Stats()` on cache.Store, sheet.Registry, lock.Manager,
  tenant.Directory, persist.Writer, ws.Conn/Handler, reader.Reader, `App.Stats`. Lingering data fixed: idle and
  replica sweep logs pruned by the janitor; reader miss records swept once per window; a closed reader frees
  its map/queue/misses; tenant revoked names and dead keys bounded (`internal/tenant/bounded.go`); emptied server
  store and reader maps replaced (Go maps never shrink). Found, not fixed: F-1 (replica keeps a revoked project's
  sheets with persistence off; D-043 recommended). `bash testing/run.sh`: `355 | 341 | 0 | 14` twice
  (14 = persist, no `REDLITE_TEST_DATABASE_URL`); leaks `-count=2` all pass, L12 `-count=3` all pass.

- 2026-10-29 — consumer conformance (D-032…D-037; next tag **v0.3.0**: new public package `reader/v1compat`;
  `reader/` and `protocol/` unchanged, no wire change): `testing/conformance/` — `consumerstore.go` (miniature of
  backendAdvo's `cachedOne`/`cachedList`/`invalidateDomain` without the second sweep/`invalidateDomainBoth`, FakeDB
  load counter), `keycorpus.go` (93 key shapes on 27 roots read from backendAdvo `cache/keys.go`), C1–C11, C13 (key
  mode), edge cases, C12 (`v1compat_test.go`), B1 (`bench_test.go`, on the corpus); `reader/v1compat` (advo_cache's
  reader surface over `reader.Reader`); `internal/cache` patient-head index (D-037, kept: on real shapes profile sweep 0.44×,
  miss sweep 0.039×, new key 1.43×); `doc/consumer/backendadvo_migration.md`, `doc/specs/s7_migration.md`. `bash testing/run.sh`:
  `327 | 314 | 0 | 13` (13 = persist, no `REDLITE_TEST_DATABASE_URL`).

- 2026-10-29 — tenancy (S8, `.claude/prompts/tenancy.md`, D-024…D-031; next tag v0.2.0): **admission** — `open` takes
  `api_key`; `REDLITE_ADMISSION=key` (production default) requires the project's key and answers every refusal
  `not authorized`; password mode is v1 unchanged (`internal/tenant`, `internal/ws/admit.go`); autocreate forced off
  outside development. **Sessions** — `internal/session` registry per node, last_seen/frames lock-free, audited.
  **Admin API** — GET /admin/projects[/{id}], POST /admin/projects/{id}/keys, DELETE /admin/keys/{id},
  POST /admin/keys/{id}/rotate, POST /admin/projects/{id}/suspend|resume, GET /admin/sessions (both nodes),
  GET /admin/sheets/{name}/sessions, DELETE /admin/sessions/{id} (`internal/api/{tenancy,sessions,touch}.go`);
  suspend/revoke/kill propagate by NOTIFY and the replica stream (control frames). **Reader** — `Config.APIKey`,
  terminal close codes 4001/4002/4003 → `LastError()` = ErrSuspended/ErrAdmitted/ErrKicked, no reconnect.
  **Peer auth** — `/replica` 401 without `REDLITE_CLUSTER_TOKEN` (before upgrade), probe and dial send it, `/health`
  hides role/epoch without it, production refuses to boot without it. Migration `000002_tenancy` (suspended_at,
  last_used_at, label, `audit_events` partitioned + BRIN); `audit` sqlc package; persist auditor. Docs: wire.md,
  release.md runbook, purpose.md "Admission and removal", s3/s4/s6 specs. `bash testing/run.sh`: `238 | 225 | 0 | 13` (after the review fixes: revoked-key race, project revoke closes 4002, admission lookup bounded at 2 s, /health omits empty fields)
  (13 = persist, no `REDLITE_TEST_DATABASE_URL`).

- 2026-10-29 — `doc/audit_2026-10-29.md`: bootstrap verified from the code (not the report): K1–K16 and R1–R12 each cited to a file/test; 0 sync.Map, 0 goroutine-per-entry, 0 fmt.Print, 51 origin comments. Remaining: `/replica` unauthenticated (the one stream defect reproduced), R6 miss window to simplify, reader eviction policy (D-024), R7 two-segment index, Postgres paths unproven.

- 2026-10-29 — bootstrap wave 5 (D-019…D-022 added to bootstrap.md mid-run, + second review): persist flush fenced on
  each op's acceptance term vs `cluster_state` (`internal/persist/writer.go` oldestTerm; `TestFlushFencesOnAcceptanceTerm`);
  writer/cleaner stop only after WS connections closed (shutdown flushes everything); `REDLITE_PERSIST_LOGGED`
  (`persist/durability.go`); NOTIFY control bus + `POST /admin/projects/{id}/revoke` (`persist/control.go`,
  `api/revoke.go`, reconcile fallback on the cleaner tick, revoked ops discarded from the writer); a dropped sheet refuses
  Apply/Join (`internal/sheet` `TestDropRefusesHeldSheet`); `REDLITE_ELECTION=advisory` (`persist/election.go`).
  `audit_events` NOT built (migration guard; proposed `000002` pair in the work folder). Full suite `176 | 165 | 0 | 11`
  (11 = persist, no `REDLITE_TEST_DATABASE_URL`).

- 2026-10-29 — bootstrap wave 4 (lead): S6 `internal/api` (POST /admin/projects, POST /admin/projects/{id}/sheets,
  GET /admin/sheets/{id}/export; sha256 keys, key_prefix logs, MemStore/DBStore, leader gate) mounted in
  `internal/app/app.go`; `protocol/admin.go`; `testing/integration/api_test.go` (autocreate=false registry path).
  Review fixes: **CRITICAL** a client- or replica-supplied epoch no longer steps a leader down — it is refused
  (`stale epoch`, reply then close) and only prompts the peer probe (`internal/replica/node.go` ObserveEpoch,
  `leader.go` register), regression `TestEdgeStaleEpoch/unauthenticated_open`; a stale-epoch reply is no longer
  lost to the step-down kick (`internal/ws/conn.go` Kick after SendAndClose, `router.go` refuse); `staticNode`
  moved to `_delete/`; stale build-process comments removed. Docs: architecture.md (goroutine table), wire.md,
  release.md (v0.1.0, the S7 Get change), README quick start, purpose.md R status, SCOPE K→test column, specs
  s1–s6, D-019…D-023, ASSUMPTIONS. `bash testing/run.sh`: see the bootstrap final report.

- 2026-10-29 — bootstrap wave 3: S4 `internal/replica` (Node: /replica register → snapshot_sheet per sheet →
  snapshot_end → stream; heartbeat; promotion at epoch+1; peer-probe fencing; hashes replicated) and the
  replica + persist wiring (`internal/app/wire_replica.go`: migrate, pool, hydration before listen, observers);
  persist: creates never dropped (FK), OpExpire as one `DeleteEntries`; integration REPLICA and PERSIST loops
  (PERSIST SKIPPED without a DB); DISCONNECT and RACE loops against the real leader; reader R6 fix (D-023).

- 2026-10-29 — bootstrap wave 2: S2 `internal/lock`, `internal/ws` (Conn: bounded send queue + one writer +
  ping; Handler/router for every v1 action), `internal/app` (New + Run under one errgroup, /health),
  `cmd/redlite` (one banner line); harness `StartLeader`/`NewReader`; KEY, SWEEP, LOCK loops and every edge case.

- 2026-10-29 — bootstrap wave 1: S1 `internal/cache` (map+RWMutex, lazy expiry, first/last-segment index:
  DeletePrefix 313 µs vs 1740 µs naive at 100k keys) and `internal/sheet` (registry, bcrypt, per-sheet mutex,
  send-queue broadcast, R6 sweep log, one janitor); S3 `reader/` (reconnect forever, delete queue + replay,
  local TTL + bound, Get(key,&T), acked sets, epoch check; deps: protocol + coder/websocket only); S5 part 1
  `internal/persist` (writer, hydrate, cleaner, epoch).

- 2026-10-29 — `doc/persist/postgres_role.md`: the two Postgres transcripts checked claim by claim against the manual; three piles (Postgres owns / Go owns / Postgres as referee); schema for 000001 grows cluster_state + audit_events; D-019…D-022 added to bootstrap.md and SCOPE pending list.

- 2026-10-29 — bootstrap wave 0 (lead): go.mod requires the seven pre-cleared modules (coder/websocket v1.8.15,
  pgx v5.11.0, x/sync v0.24.0, x/crypto v0.58.0, google/uuid v1.6.0, golang-migrate v4.20.1, x/time v0.16.0);
  `protocol/messages.go` (every v1 frame + id, version, protocol_version, epoch, seq, expires_in_ms, keys,
  base_seq, replica snapshot types) with `messages_test.go` pinning the v1 field names; `internal/logger`
  ReplaceAttr masking (password, password_hash, value, key_hash, api_key, database_url) + test;
  `internal/config` without package globals, `Default()`, every new REDLITE_* (ports default :8095/:8096);
  `db/migration/000001_init.{up,down}.sql`, `db/query/{registry,entries,cluster}`, `sqlc.yaml`, generated
  `db/sqlc/*`, `db/store.go` (Store + ExecTx + Batch + embedded Migrate); `.air.toml`, `Makefile`
  (backendAdvo target names + dev-pair + bench), two-stage `Dockerfile`, `docker-compose.yml`
  (postgres + leader + replica); `testing/integration/harness.go` (raw client, cuttable proxy, unique
  sheets, persist skip); `internal/cache/entry.go`, `internal/sheet/op.go` (cross-package contracts);
  D-002…D-018 in DECISIONS; ASSUMPTIONS rows; wire.md "Changes since v1". Tests: `4 | 4 | 0 | 0`.

- 2026-10-29 — bootstrap prep: `.claude/prompts/bootstrap.md` (D-002…D-018 decided, 4-agent plan, done-means); `migration-guard.sh` (new pairs only, db/sqlc generated-only); settings: `sqlc generate|vet|compile` allowed, `go get` allowed only for the 7 pinned modules, db/migration deny replaced by the guard.

- 2026-10-29 — `doc/assessment_2026-10-29.md`: advo_cache read in full (ws/handler.go, sheet, cache, replica, reader); 22-row pattern comparison; eight new defects K9–K16 (concurrent-write panic path, no ordering, unfenced promotion, replica-sheet password takeover, sheet deleted on last disconnect, open-creates-sheet, no limits/keepalive, zero tests). SCOPE: K9–K16, D-012/013/014 pending, S2/S4/S5/S6 scope lines updated; S6 now required.

- 2026-10-29 — advomedix-v2 read-only: `.claude/hooks/advomedix-readonly.sh` (PreToolUse Bash|Edit|Write|MultiEdit|NotebookEdit; 23 cases checked: 7 allow, 16 block), `settings.json` deny Edit/Write/NotebookEdit + secret reads on advo_production/**, `settings.local.json` `additionalDirectories` + Read allows for advo_cache, backendAdvo, .gitea/workflows, doc/backend. `rm`/`rmdir`/`unlink` moved from allow to deny (policy is `mv -n` to `_delete/`).

- 2026-10-29 — purpose + agent prompt: `doc/purpose.md` (backendAdvo usage researched: call-site counts, 3 instances on one
  sheet, R1–R12 from `backendAdvo/cache/store.go:8-87`); `.claude/prompts/request.md` (request header adapted from the
  advomedix one: dependency + migration checkpoints replace SQLC, CONTRACTS replace the uniform contract, seven
  integration loops replace CRUD/UI loops). `doc/protocol/wire.md`: `ttl` (minutes) added to `set` and the
  compatibility list — it was missing. Delete policy switched to move-to-`_delete/` in CLAUDE.md to match.

- 2026-10-29 — published: github.com/CrimsonCoder42/red_lite_cache (public), `main` @ 71534ec, tag v0.0.1. D-001 accepted.

- 2026-10-29 — reference knowledge: `examples/` (4 raw transcripts, Devin) indexed by `examples/README.md`;
  `doc/reference/codeheim_patterns.md` (46 code-heim repos cloned and read; two found not to compile:
  go_82 both servers miss `import "sync"`); `doc/reference/distributed_cache.md` (Anthony GG stream
  distilled, claims checked); `doc/reference/README.md`. `.claude/CLAUDE.md` "Reference sources" (three
  layers, cite-the-origin rule), `/work` Phase 0/C/D read and cite references, reviewer dimension 6b
  provenance, `/cleanup` §6 fixes a wrong reference row, every `rules/backend/*.md` carries an Origin line.
  SCOPE: D-011 (websocket library) added to pending decisions.

- 2026-10-29 — scaffold (S0): `.claude/` (CLAUDE.md, settings, hooks, rules, skills, reviewer), `doc/`
  (scope, specs template, decisions D-000, assumptions, protocol, persist, architecture, release),
  `testing/` (run.sh, integration harness stub, probe templates), `go.mod` Go 1.27, package stubs that
  build, `cmd/redlite` health server, `.env.example`, Dockerfile, docker-compose. No behavior yet.
