# code-heim patterns — base knowledge, checked

_Source: 46 repositories cloned from https://github.com/code-heim on 2026-10-29 (the cache,
websocket, concurrency, config, logging, persistence and testing episodes; GORM/Excel/PDF/Kafka and the
other unrelated episodes were skipped). Each repo is a 20–80 line YouTube companion written between
Nov 2023 and Apr 2025 on Go 1.18–1.23. They are **teaching snippets, not production code**, and this
file says for each one what the pattern is, what in it is still right, what is dated or wrong, and what
red_lite_cache does instead. Treat it as the starting idea; the decision is in `doc/DECISIONS.md`._

**How to cite one in code or a spec:** `// after code-heim go_11_cache_sync_map, with X changed because Y`.

Upstream versions checked the same day: gorilla/websocket v1.5.3 · coder/websocket v1.8.15 ·
pgx v5.11.0 · golang-migrate v4.20.1.

---

## Tier 1 — directly about this project

### go_11_cache_sync_map — "Caching with sync.Map" (Go 1.20)

**Pattern.** A package-level `sync.Map`, `Load` on the way in, `Store` on a miss, a goroutine that
sleeps the TTL then `Delete`s.

**What it gets right.** Cache-aside is the right shape: check, compute, store, return.

**What is wrong in the 40 lines** — all three are bugs advo_cache inherited:

| Line | Defect | Why it matters here |
|---|---|---|
| `if cacheResult, ok := cache.Load(nInt); ok { c.JSON(...) }` with no `return` | On a hit it writes the response **twice** and recomputes anyway | Any "hit" path that falls through is a correctness bug, not a perf one |
| `go func(){ time.Sleep(5*time.Minute); cache.Delete(nInt) }()` | One parked goroutine per entry; a re-store does not reset the timer, so a refreshed value is deleted on the OLD schedule | advo_cache `cache/cache.go:44-51` is this exact code. K5 in SCOPE.md |
| `sync.Map` as the default | The `sync.Map` doc says it is for append-only or disjoint-key workloads; a TTL cache is neither | D-002 |

**Here.** `internal/cache`: `map[string]entry` + `sync.RWMutex`, `expiresAt` in the entry, lazy expiry on
`Get`, one janitor per process. Rule: `.claude/rules/backend/go-caching.md`.

### go_45_memcache — "Caching in Go: Memcached" (Go 1.22)

**Pattern.** `CacheData(key, ttl, fn)` — a read-through helper: `Get`; on miss call `fn`, `Set`, return.
Handler passes a closure that loads from the DB and marshals.

**What it gets right.** The read-through helper is the shape `backendAdvo/cache/shared.go` uses
(`cachedOne`/`cachedList`): the domain supplies a key and a loader and never writes `Get`/`Set` itself.
The key grammar `blog:<id>` is the `<root>:<id>` prefix shape.

**What is dated or wrong.**
- `fn interface{}` + `reflect.ValueOf(fn).Call(...)` to accept "any function returning []byte". Go 1.18
  generics make this `func CacheData[T any](key string, ttl time.Duration, load func() (T, error)) (T, error)`.
  Reflection here buys nothing and costs a panic path.
- `panic(err)` on a cache `Set` failure. A cache write failure is a Warn and a fall-through, never a crash.
- Errors are cached as misses only by accident (`err != nil` → recompute). The loader's own error is
  swallowed: `blogBytes, _ := json.Marshal(blog)`.
- The 2 s `time.Sleep` to demo latency is inside the loader; fine for a video, never in a reference.

**Here.** The reader exposes `Get` only; read-through stays on the consumer side (backendAdvo already has
it). `reader.Get` decodes into the caller's `T` with generics, no reflection.

### go_8_redis_lock — distributed lock with SETNX (Go 1.18)

**Pattern.** `SETNX key "1" EX ttl` to acquire, `DEL key` to release.

**The defect worth remembering.** Release is `DEL key` with no owner check. Client A acquires, stalls past
the TTL, the key expires, client B acquires, A wakes and `DEL`s **B's** lock. advo_cache `lock/lock.go`
already fixed this with an owner on the lock and `unlock` refusing a non-owner — keep that. Redis's own
docs make the same point (release must be compare-and-delete on a unique token).

**Here.** `internal/lock`: owner = connection id, TTL, cleanup on disconnect, `unlock` from a non-owner
returns `not lock owner`. Same as advo_cache; the test asserts the A-stalls-B-acquires-A-unlocks case.

### go_12_gorilla_websocket — echo server (Go 1.20, gorilla v1.5.x)

**Pattern.** `Upgrader` → `ReadMessage` loop → `WriteMessage` echo.

**Dated / wrong for a server.**
- `CheckOrigin: func(*http.Request) bool { return true }` disables origin checks. For a service that only
  talks to backends it is tolerable, but it has to be a stated decision, not the default.
- No write pump. gorilla allows **one concurrent writer per connection**; a broadcast fan-out that calls
  `WriteMessage` from the sheet goroutine while the read loop replies is a data race. advo_cache
  `sheet/sheet.go` broadcasts from whichever goroutine performed the write — check it under `-race`.
- No read/write deadlines, no ping/pong, no close handshake. A half-open socket is never detected.
- gorilla was archived in Dec 2022 and un-archived under new maintainers in 2023; v1.5.3 (Oct 2024) is the
  current tag. It is maintained again but conservatively.

### go_75_coder_websocket — echo with coder/websocket (Go 1.23)

**Pattern.** `websocket.Accept` → `conn.Read(ctx)` / `conn.Write(ctx, ...)`. Every call takes a context.

**What it gets right.** The context on every read and write is the shape this project wants: cancel the
server ctx and every connection goroutine unwinds. `conn.Close(status, reason)` does the close handshake.
coder/websocket is the renamed `nhooyr.io/websocket` (moved to the coder org, 2024); v1.8.15 is current,
zero dependencies, and it is **safe for concurrent writers** (it serialises internally), which removes the
write-pump requirement gorilla has.

**Dated / wrong.** `InsecureSkipVerify: true` in `AcceptOptions` turns off origin checking — same note as
gorilla. The client does blocking `Read` right after `Write`, which is an echo-demo shape, not a client.

**Decision this forces (D-011, before S2).** gorilla (advo_cache's choice, battle-tested, needs a write
pump and manual ping/pong) vs coder/websocket (ctx-native, concurrent-write-safe, `wsjson` helper, smaller
API). Recommendation: **coder/websocket**. Alternative: gorilla, loses on the ctx plumbing and the write
pump we would otherwise write and test ourselves. The reader is a library a consumer embeds, so its
dependency footprint matters; coder has none.

---

## Tier 2 — concurrency building blocks the server is made of

### go_20_mutexes_and_confinement (Go 1.20)

Three versions of "sell 500 tickets to 2000 goroutines": racy, mutex, confinement (one owner goroutine, a
request channel). **Right:** this is exactly the choice in `go-concurrency.md` — confine when one goroutine
can own the state, mutex otherwise. **Defect in the confinement version:** `manageTicket` never returns
(`for { select {...} }` with no exit after `doneChan`), so it leaks; and `tickets` is shared by pointer with
`main`, which reads it after `wg.Wait()` while the owner may still be writing. Every owner goroutine here
takes a `ctx` and returns on `ctx.Done()`.

### go_18_worker_pool (Go 1.20)

`tasksChan` sized to `len(Tasks)`, N workers `range` the channel, `wg.Add(len)` up front, `close` after
enqueue. **Right:** bounded concurrency, close-to-drain, `Add` before the goroutines. **Dated:** no ctx,
no error return, `wg.Done()` inside the worker instead of a `defer`; Go 1.25 `wg.Go` replaces the
`Add`/`Done` pair (`go-concurrency.md`). The persist writer is a one-worker version of this with a
flush-on-size-or-interval `select`.

### go_21_goroutines_pipeline, go_25_fan_out_fan_in, go_62_ordone_tee_channel

Pipeline: each stage `go func(){ for v := range in { out <- f(v) }; close(out) }()`. Fan-out: one goroutine
per job, collect N results. `orDone`: wrap a channel so a `done` signal unblocks the consumer.
**Right:** the stage shape and `orDone` are the canonical ones (Cox-Buday). **Missing in all three:** no
ctx, so a stuck downstream blocks every upstream forever; fan-out spawns one goroutine per item with no
bound; `tee` deadlocks if either consumer stops reading. `go-pipeline.md` and `go-fan-out-fan-in.md`
carry the ctx-and-bound versions; the replica stream and the persist writer are pipelines.

### go_61_err_group (Go 1.23), go_67_conc (Go 1.23)

`errgroup.WithContext` + `SetLimit(2)`; sourcegraph `conc/pool` `WithErrors().WithContext().WithMaxGoroutines()`.
**Right:** first error cancels the siblings; a limit bounds the fan-out. **Defect in go_61:** `fetchPage`
checks `ctx.Done()` once at the top then calls `http.Get(url)` without the ctx, so cancellation never
reaches the request — use `http.NewRequestWithContext`. **Here:** `errgroup` (x/sync, already needed for
`singleflight`) for startup/shutdown of the goroutine set in `cmd/redlite`; no `conc` — one dependency
that does the same job is enough.

### go_51_context

Four demos: cancel, timeout, deadline, key/value. **Right:** `defer cancel()` always. **Note:** the
key/value demo uses string keys; use an unexported struct key type so two packages cannot collide. The
only values that belong in a ctx here are the connection id and the sheet id for logging.

### go_54_sync_pool

`sync.Pool` of 1 KiB `[]byte`. **Right:** for per-frame scratch buffers in the WS read loop. **Caveat:**
the pool is cleared on GC, `Put` of a grown slice retains the larger backing array — cap what goes back.
Only after a benchmark shows allocation pressure (REFACTOR/PERF track).

### go_27_panic_recover

`recover()` → log → cleanup → `panic(r)` re-panic. **Right:** log at Error with the module and the value,
never a bare `recover()` (`go-logging.md`). A connection goroutine recovers, logs, closes that one socket,
and does **not** re-panic — one bad client must not take the process down.

### go_56_generator, go_60_iter (Go 1.23)

Channel generator (old) vs `iter.Seq[V]` range-over-func (new). **Here:** `go-iterators.md` — a sheet dump
or a prefix scan is an `iter.Seq2[string, []byte]`, never a channel and never a materialised slice.

---

## Tier 3 — infrastructure the project touches

### go_74_pgx — pgxpool (Go 1.23, pgx v5)

`pgxpool.New`, `pool.Begin` + `defer tx.Rollback` + `tx.Commit`, `Query`/`rows.Scan`, `QueryRow`.
**Right:** the tx shape (`defer Rollback` is a no-op after `Commit`). **Dated:** `context.Background()`
everywhere; a connection string in code; `log.Fatalf` on every error. **Here:** `go-persist.md` — one pool
in `cmd/redlite` from config, every call takes the caller's ctx, a batch is one tx, `pgx.Batch` for the
flush. pgx v5.11 is current.

### go_77_rate_limit, go_82_rate_limiting_revised (Go 1.23)

`golang.org/x/time/rate` per-IP limiter in a `map` + `Mutex` (v1) and in a `sync.Map` with `LoadOrStore`
(v2). **Right:** token bucket per identity. **Defects:** the `go_82` files **do not compile** — both use
`sync` without importing it (a reminder that these repos were never built in CI). The `sync.Map`
version calls `rate.NewLimiter` on **every** request and throws it away on hit (the `LoadOrStore`
cheap-argument rule in `go-caching.md`). Neither evicts idle IPs, so the map grows forever. **Here:**
`go-rate-limiting.md` — key on sheet (identity), not IP; evict idle limiters from the janitor.

### go_26_slog

`ReplaceAttr` to mask `password`-like keys; `slog.With(slog.Group(...))` child loggers; multi-writer.
**Right:** `ReplaceAttr` masking is a cheap safety net for the "never log a value or password" rule —
`internal/logger.Configure` installs one for `password`, `value`, `key_hash`, `database_url`. Child
logger per connection (`logger.L.With("conn", id, "sheet", s)`) is the shape for `internal/ws`.

### go_19_custom_logger

A hand-rolled leveled logger with ANSI colours — the shape advo_cache `logger/logger.go` copied. **Dated:**
`log/slog` has been in the stdlib since Go 1.21. D-007.

### go_17_env_variables, go_72_viper_config, go_31_yaml_config

`os.Getenv` scattered through models (go_17); Viper with `.env` + env override (go_72, advo_cache's
choice); YAML (go_31). **Here:** D-006 — a 100-line stdlib `.env` + env loader in `internal/config`
(already written) over Viper's 20 transitive dependencies, because the reader is a library consumers
embed and must not drag them in. Config read **once**, typed, validated; the hook rejects `os.Getenv`
anywhere else.

### go_70_must_pattern

`Must[T](v, err) T` panics on error. **Right:** at `init`/`main` for things that cannot fail at runtime
(compiling a regexp). **Wrong:** `MustQueryParam` panicking on a missing HTTP param — user input is never
a panic. Nothing in the request/frame path uses `Must`.

### go_43_net_http_middleware

Closure middleware and `http.Handler` wrapper struct. **Right:** the wrapper shape for `internal/api`
(API-key auth, request logging). stdlib `net/http` with Go 1.22 method patterns (`"GET /health"`), no Gin.

### go_53_http_testing, go_83_http_testing_2 (Go 1.22)

`httptest.NewRecorder` + `testify`; `//go:build integration` tag to split suites. **Right:** `httptest`
is how `testing/integration` boots a leader on a free port. **Dated:** the integration test is a stub
(`fmt.Println("Dummy")`). **Here:** no `testify` — stdlib `testing` + `cmp` is enough and one less
dependency; build tags are not needed because persist tests skip on a missing env var instead.

### go_80_tcp_udp_sockets

Raw `net.Listen` + `Accept` + `SetReadDeadline` loop. The anthdm stream (`distributed_cache.md`) builds on
this layer. **Defects:** `Accept` error is logged then `conn` (nil) is used; a 1 KiB `Read` with no
framing, so a message split across two reads is corrupted. **Here:** WebSocket gives us framing, close
handshake and ping/pong for free; D-000 keeps the wire on WS for compatibility with the shipped reader.

### go_69_bloom_filter

`bits-and-blooms/bloom` in front of a DB "does this user have access" check. **Relevant:** a per-sheet
bloom filter is a cheaper **cache penetration** guard than `__NULL__` placeholders for keys that are asked
for once and never again — but it cannot be invalidated without a rebuild. Keep `set_null` (v1 wire
compat); bloom is a TODO.md candidate, not a v1 feature.

### go_37_background_work, go_49/50_task_scheduling, go_68_asynq, go_66_tunny

Redis-backed job queues (`gocraft/work`, `asynq`), cron scheduling, a goroutine pool. **Not used:** the
persist writer is one goroutine with one channel; a queue system is a second runtime to operate. Noted so
nobody re-proposes it without a reason.

### go_41_dependency_injection, go_40_interfaces_generics, go_14_builder_pattern, go_7_go_decorator, go_85_first_class_functions_closures

Constructor injection, small interfaces, functional options. **Here:** `reader.New(Config)` is a struct
config, not a builder; `internal/*` constructors take their dependencies (`sheet.NewManager(cache.Factory,
logger)`) — no globals except `logger.L`.

### go_52_protobuf, go_55_grpc, go_63_cobra_cli, go_57_build_tags, socket_io_emitter

Out of scope for v1 (JSON wire for compatibility; one binary with env config, no subcommands; no
Socket.IO). Protobuf frames are the obvious v2 wire if size ever matters — the `protocol/` package is
where that would land, which is why it exists as its own package.

---

## The pattern across all 46

Every repo shares the same gaps, and they are the gaps advo_cache shipped with:

1. **No `context.Context`** on anything long-lived — goroutines cannot be stopped.
2. **Goroutine-per-item** with no bound (expiry, fan-out).
3. **`panic`/`log.Fatal` as error handling** in library-shaped code.
4. **`sync.Map` by reflex** where a plain map and a mutex is what the doc recommends.
5. **Package-level globals** (the cache, the pool, the limiter map) instead of injected dependencies.
6. **Never built in CI** — two of the 46 do not compile.

The `.claude/rules/backend/*.md` files exist to stop each of these from being copied forward. When a
code-heim shape is used, cite it and name the change; when a rule here contradicts the repo, the rule wins.
