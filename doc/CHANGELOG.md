# Changelog

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
