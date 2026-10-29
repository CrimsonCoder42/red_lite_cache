# Assessment — advo_cache against the examples and production patterns

_Written 2026-10-29 from the code, not the README: `advomedix-v2/advo_cache` (2,841 lines, 0 test files),
its consumer `backendAdvo/cache/store.go`, the four `examples/` transcripts (distilled in `doc/reference/`),
46 code-heim repos, and the production references cited inline. Every claim about advo_cache cites file:line.
This is the input to the S1–S6 specs; SCOPE.md's defect list is updated from it (K9–K15)._

## The short answer

**Succeeding:** the product idea and the API shape. One leader, an embedded reader with O(1) local reads,
invalidation broadcast to every collaborator, tenant isolation by sheet, a hot standby. backendAdvo built
79 cache files and ~400 call sites on it and the shape held. Nothing in the examples or the production
references argues for a different *shape*; they argue for a different *build*.

**Falling short:** everything that happens when something goes wrong. Disconnects drop deletes, a
partition makes two leaders, the server can panic on a concurrent write, the full keyspace is scanned per
sweep, and there is no test for any of it.

**Hurting most, in order:** (1) no tests and no `-race`, which is why the next four shipped; (2) the
consumer carrying the library's correctness (the 80-line banner and ~150 lines of workaround in
`store.go`); (3) split-brain on promotion with no fencing; (4) a concurrent-write panic path in the
server; (5) a module layout where importing the reader imports the server.

---

## 1. Where advo_cache succeeds

| What | Evidence | Keep |
|---|---|---|
| Embedded reader, local O(1) `Get`, writes forwarded | `reader/reader.go:157`, `:206-237` | The whole value proposition. R-rows in `purpose.md` are about making it *reliable*, not changing it |
| Invalidation broadcast, sender excluded | `sheet/sheet.go:111-148`, `ws/handler.go:284-306` | Yes. This is what the stream's `sendToFollowers` wanted to be |
| Tenant isolation by sheet, bcrypt password | `sheet/sheet.go:39-52,66-82`, `ws/handler.go:222-255` | Yes, with a registry in front (§3.6) |
| Owner-checked locks with TTL and disconnect cleanup | `lock/lock.go`, `ws/handler.go:137-150` | Yes. Already better than code-heim `go_8_redis_lock` (releases without owner check) |
| Leader → replica stream, heartbeat, promotion | `ws/handler.go:506-632`, `replica/client.go:243-294` | The mechanism, yes; the election, no (§3.3) |
| Three primitives the consumer actually needs: `Delete`, `DeletePrefix`, `DeleteSuffix` | `purpose.md` call-site table | Yes. The key grammar `<root>:patient:<uuid>:<view>` is built on exactly these |
| Reader has a write mutex | `reader/reader.go:108` (`writeMu`) | Yes — the server lacks the same thing (§3.4) |
| One `.env`, typed config, role by env var | `config/config.go` | Shape yes; Viper no (§5) |

## 2. The pattern comparison

What each dimension looks like in advo_cache, what the examples did, what production does, and the verdict.

| Dimension | advo_cache | Examples (stream / code-heim) | Production pattern | Verdict |
|---|---|---|---|---|
| **Store** | `sync.Map` per sheet (`cache/cache.go:19-20`) | stream: map+RWMutex; code-heim go_11: `sync.Map` | plain map + `RWMutex`, sharded if contended ([sync.Map doc](https://pkg.go.dev/sync#Map), [go#17973](https://golang.org/issue/17973)); groupcache/ristretto shard | **Change** (D-002) |
| **Expiry** | goroutine + `time.Sleep` per `Set` (`cache/cache.go:45-51`) | stream and go_11: identical | expiry in the entry, lazy on read, one janitor (go-cache, ristretto); or a timing wheel at scale | **Change** (K5). A re-`Set` does not cancel the old timer, so a refreshed value dies on the old schedule |
| **Concurrent socket writes** | server: `conn.WriteMessage` from any client's goroutine with no mutex (`sheet.go:123,145`, `handler.go:264,501,579,620,654`) | go_12 gorilla echo: one goroutine, so never hit | gorilla: *one concurrent writer*; the standard answer is a write pump per conn (gorilla chat example) or a library that serialises (coder/websocket) | **Fix** (K9). Two backends writing at once can produce gorilla's `concurrent write to websocket connection` panic |
| **Framing / limits** | no `SetReadLimit`, 1 KiB buffers (`handler.go:64-70`) | none set limits | `SetReadLimit` always; max frame in config | **Add** |
| **Keepalive** | no ping/pong, no deadlines; heartbeat is leader→replica only | go_75 (coder) uses ctx per read | ping/pong with read deadline on every conn, so half-open sockets die | **Add** |
| **Wire types** | `Request` is one struct with every field of every action (`handler.go:24-38`); replica types in a second package; reader decodes into `map[string]interface{}` (19 sites) | stream: one `Message` struct, same complaint | one package of typed messages; `json.RawMessage` for the payload | **Change** (K7 → `protocol/`) |
| **Delete while disconnected** | returns `ErrNotConnected`, reconnect reloads server state (`reader.go:242,266,295,443`) | stream: no reconnect at all | queue + replay on reconnect, or version/sequence numbers so a stale snapshot is detectable | **Fix** (R2). Today the *consumer* queues them |
| **Reconnect** | 3 attempts per server then gives up (`reader.go:530-584`) | stream: `log.Fatal` | forever with capped exponential backoff + jitter | **Fix** (R1) |
| **Join / catch-up** | full `sheet_state` dump on open; `full_state` of every sheet to a replica in one frame (`handler.go:486-504`) | stream: none | snapshot + stream, chunked, with a sequence number so the follower knows where the stream resumes | **Change** (K6). One JSON frame of every tenant's data |
| **Ordering** | each client goroutine broadcasts directly; two writes to the same key from two clients can arrive at a third in either order | stream: `go sendToFollowers` per write (worse) | one sequencer per sheet (a confinement goroutine or a per-sheet mutex around apply+broadcast) | **Fix** (K10) |
| **Leader failure** | replica promotes on heartbeat timeout; old leader never steps down; no fencing token (`replica/client.go:273-294`, grep: no epoch anywhere) | stream: explicitly out of scope ("you need consensus") | static leader + one replica is fine **if** promotion is fenced: an epoch the readers and the old leader can compare; or an external lock (Postgres advisory lock) decides who leads | **Fix** (K11). Today a Tailscale blip = two leaders, divergent, never reconciled |
| **Promoted-replica auth** | a replica sheet has no password; the first `open` after promotion *sets* it (`sheet.go:66-78`) | — | credentials are replicated with the data, or verified against a registry | **Fix** (K12). Anyone can claim any tenant after a failover |
| **Sheet lifecycle** | deleted when the last collaborator leaves (`handler.go:160-164`) | — | a cache can be ephemeral, but then "persistence" has nothing to persist; a sheet must outlive its connections | **Decide** (K13). This is the first thing the Postgres design collides with |
| **Sheet creation** | first opener creates it with whatever password (`manager.go:27-64`) | — | a project registry: sheets are created by an admin API, `open` only joins | **Change** (K14). Required for "other projects sign in" |
| **Value handling** | `interface{}` everywhere; server re-encodes JSON per broadcast; reader hands back `map[string]any`, consumer double-marshals (`store.go:69-77`) | code-heim go_45: reflection to call a loader | store `json.RawMessage` bytes, never decode on the server, decode once in the reader into the caller's `T` | **Change** (R8) |
| **Sweep cost** | `Range` over the whole sheet per `DeletePrefix` (`cache.go:63-74`) | — | a prefix index (sorted keys / radix) or per-root sub-maps keyed by the grammar's first segment | **Change** (R7) |
| **Backpressure** | none: a slow collaborator blocks the broadcasting goroutine on `WriteMessage` | stream: same | bounded per-conn send queue; drop the connection (it will resync) rather than stall the sheet | **Add** |
| **Shutdown** | none: `ListenAndServe` with no signal handling, heartbeat ticker never stops (`main.go`, `handler.go:599-607`) | all examples: same gap | `signal.NotifyContext`, `Server.Shutdown`, every goroutine on a ctx | **Add** (done in S0 skeleton) |
| **Config** | Viper + `.env` (`config/config.go`) | go_72: Viper; go_17: scattered `os.Getenv` | stdlib env, read once, typed, validated | **Done** (D-006) |
| **Logging** | hand-rolled ANSI logger (`logger/logger.go`), 150-line how-to banner on boot (`main.go:95-260`) | go_19: same; go_26: `slog` | `slog`, structured, no values | **Done** (D-007) |
| **Module boundary** | one module; `reader/` imports nothing of the server but shares `go.mod`, so backendAdvo's `go.mod` carries Viper, bcrypt, gorilla, and a `replace` to a sibling directory | — | `internal/` for the server; the public packages pull in as little as possible; consumers pin a tag | **Done** (layout) — verify with `go mod why` in S3 |
| **Tests** | **zero** `_test.go` files; no `-race` ever run; no benchmark behind the "O(1)" claims | stream: "are we going to test this? probably not" | unit + integration + `-race` in CI; benchstat for perf claims | **The root cause** |

## 3. Where it falls short — ranked by harm

### 3.1 No tests, no race detector (the root cause)

Zero test files in 2,841 lines. Every defect below is one a basic integration test would have caught:
a two-reader test catches dropped deletes; `-race` on a two-client test catches the unsynchronised
broadcast; a kill-the-leader test catches the promotion gap. The stream said out loud what advo_cache
did silently: "are we going to test this? probably not." **This is why the other four exist.**

### 3.2 The consumer carries the library's correctness

`backendAdvo/cache/store.go` has an 80-line banner (lines 8-87) listing the engine's defects and ~150
lines of compensation: a dropped-sweep queue with generations, a `linkReady` gate on every read, a second
sweep 2 s after the first, a double JSON pass for isolation. That code is in the wrong repository. It is
the clearest measure of the gap: **R1–R12 in `purpose.md` are the lines backendAdvo gets to delete.**

### 3.3 Split-brain on promotion (K11)

Leader on the NUC, replica on DigitalOcean, link over Tailscale. On a heartbeat timeout the replica
promotes itself (`replica/client.go:264-266`) and the leader is never told, never steps down, and there
is no epoch or fencing token anywhere in the code. Readers fail over on their own schedule
(`reader.go:530-584`), so after a blip some backends write to the NUC and some to DO. Nothing
reconciles when the link returns. The stream called this out as the hard part and left it out on
purpose; advo_cache shipped the half that makes it dangerous.

What production does without Raft: **fence it.** An epoch number that increments on promotion, carried
in every replicate frame and every reader's `open`; an old leader that sees a higher epoch steps down to
replica; a reader that sees a lower epoch refuses the writer. Or let something that already has consensus
decide — a Postgres advisory lock (`pg_try_advisory_lock`) held by whichever instance is leader, which the
persistence database gives us for free in S5. Either is a D-id before S4.

### 3.4 Concurrent write panic path (K9)

Client A's read-loop goroutine handles A's `set`, then calls `Sheet.BroadcastSetMsg`, which writes to
B's and C's connections (`sheet.go:140-147`). At the same moment B's goroutine is writing B's own response
to B's connection (`handler.go:652-655`). gorilla/websocket allows one concurrent writer per connection
and panics otherwise. The reader got a `writeMu` (`reader.go:108`); the server did not. With three
backends it is a matter of load. coder/websocket serialises writes internally, which is half of D-011;
the other half is a per-connection send queue so a slow collaborator cannot stall a sheet.

### 3.5 The module ships the server to every consumer

`backendAdvo/go.mod:6,86` pins `advo_cache v0.0.0` via `replace => ../advo_cache`. That means
backendAdvo's build depends on a sibling checkout and carries the server's dependency set. "Finicky" is
the right word: no tag, no version, no way to build backendAdvo from a clean clone. The S0 layout fixes
the boundary (`internal/` for the server, `reader/` + `protocol/` public); the S3 acceptance check is
`go mod why -m` from a consumer showing only the websocket library and `x/sync`.

### 3.6 Multi-project is not possible yet (K12, K13, K14)

The vision — other projects sign in, a Postgres pair behind it — needs three things the code does not have:

1. **A registry.** Today the first `open` of a never-seen sheet name creates it with whatever password was
   sent (`manager.go:27-64`). Any client that can reach the port can create a tenant. Multi-project means
   sheets are created by an admin API with an API key (`CACHE_PERSISTENCE_REPORT.md` Part 3 already
   designs this), and `open` only joins.
2. **Credentials that survive failover.** A replica sheet has no password hash and the first opener after
   promotion sets it (`sheet.go:66-78`). That is a tenant takeover on every failover. Hashes replicate
   with the data, or auth is checked against the registry.
3. **A sheet that outlives its connections.** Deleting the sheet when the last collaborator leaves
   (`handler.go:160-164`) made sense for an ephemeral cache; with persistence it is a bug (hydrate on
   boot, then the first disconnect wipes it). Sheet lifecycle becomes: created by registry, hydrated from
   Postgres, evicted from memory only by policy, never by "last one out".

## 4. What the examples got right that advo_cache skipped

- **Stream:** separate the storage interface from the transport so the engine is testable without a
  socket; map + `RWMutex`; the leader applies locally then forwards. All three are in the S1 spec.
- **code-heim go_20 confinement:** one owner goroutine per sheet that applies writes and broadcasts in
  order is the simplest fix for K10 (ordering) and K9 (one writer per sheet means one place to serialise).
- **code-heim go_75 coder/websocket:** ctx on every read and write; close with a status code.
- **code-heim go_26 slog:** `ReplaceAttr` masking so a value or password cannot reach a log line.
- **code-heim go_61 errgroup:** the goroutine set in `cmd/redlite` (listener, janitor, heartbeat, persist
  writer, cleaner) started under one `errgroup.WithContext`; first failure cancels the rest.
- **code-heim go_18 worker pool:** the persist writer is one worker with a bounded channel.

What the examples got **wrong** that advo_cache copied: goroutine-per-entry expiry (go_11 and the stream),
`sync.Map` by reflex (go_11, go_82), no ctx anywhere, no tests.

## 5. Simplicity — what to cut so it stays easy to use

The vision is "more projects run this". Simple to *operate* and simple to *embed* are the two tests.

| Cut | Why |
|---|---|
| The 150-line stdout how-to (`main.go:95-260`) | It is documentation; `doc/protocol/wire.md` is where it lives. Boot prints one line |
| Viper and its 20 transitive deps | A library consumers embed must not drag a config framework in. 100 lines of stdlib do it (S0) |
| `GetString` / `GetMap` / `IsNull` typed getters on the reader | One generic `Get[T]` (or `Get(key, &target)`) replaces them; `IsNull` stays for wire compat but is not promoted |
| `dump` as a client action | O(n) over tenant data on a client socket; make it an admin API endpoint with an API key |
| `jitter` as a per-request flag | Jitter is a server policy (`REDLITE_CACHE_JITTER_PERCENT`); the field stays accepted and ignored |
| `register_replica` as a client action on the same socket | A replica is a peer, not a client; it gets its own path (`/replica`) and its own auth, so client auth and replica auth cannot be confused |
| The second broadcast type (`BroadcastSet` vs `Broadcast`) and the second replica type package | One `protocol` package, one envelope |

What stays simple on purpose: one binary, role by env var, one `.env`, JSON on the wire (v1 compat;
protobuf is a v2 question), a reader whose public surface is the nine methods backendAdvo calls.

## 6. What this changes in the specs

- **SCOPE.md:** K9–K15 added (below). S2 gains "per-connection send queue + read limit + ping/pong";
  S4 gains "fenced promotion (epoch or advisory lock) — D-id"; S5 gains "sheet lifecycle decoupled from
  connections"; S6 becomes the registry (create sheet, issue key) and is **required** for multi-project, not
  optional.
- **New pending decisions:** D-012 promotion fencing (epoch in-protocol vs Postgres advisory lock);
  D-013 sheet lifecycle (registry-owned, hydrated, policy-evicted); D-014 per-sheet sequencer
  (confinement goroutine vs mutex around apply+broadcast).
- **Specs' acceptance criteria** come from the seven loops in `.claude/prompts/request.md`; each K and R
  row maps to at least one loop. The DISCONNECT, RACE and REPLICA loops are the ones advo_cache would fail today.

### New defect rows for SCOPE.md

| # | advo_cache defect | Where | Fix |
|---|---|---|---|
| K9 | Server writes to a connection from many goroutines with no mutex → gorilla concurrent-write panic | `sheet/sheet.go:123,145`; `ws/handler.go:264,501,579,620,654` | per-conn send queue / coder/websocket (D-011) |
| K10 | No ordering guarantee: each client goroutine broadcasts directly | `ws/handler.go:284-306` | per-sheet sequencer (D-014) |
| K11 | Promotion without fencing: old leader never steps down, no epoch → split-brain on partition | `replica/client.go:243-294`; no epoch anywhere | D-012 |
| K12 | Promoted replica's sheets have no password; first opener sets it | `sheet/sheet.go:66-78` | hashes replicate; registry auth |
| K13 | Sheet deleted when the last collaborator disconnects | `ws/handler.go:160-164` | registry-owned lifecycle (D-013) |
| K14 | Any client can create any sheet with any password | `sheet/manager.go:27-64` | registry + admin API (S6) |
| K15 | No read limit, no ping/pong, no deadlines, no backpressure | `ws/handler.go:64-70`, broadcast paths | S2 |
| K16 | Zero tests, no `-race`, no benchmarks | repo-wide | every slice ships with its loop tests |

## Sources

- [sync.Map documentation — when to use it](https://pkg.go.dev/sync#Map)
- [golang/go#17973 — RWMutex scales poorly with CPU count](https://golang.org/issue/17973)
- [gorilla/websocket — concurrency: one concurrent writer](https://pkg.go.dev/github.com/gorilla/websocket#hdr-Concurrency)
- [gorilla/websocket chat example — write pump per connection](https://github.com/gorilla/websocket/tree/master/examples/chat)
- [coder/websocket — concurrent Write is safe](https://pkg.go.dev/github.com/coder/websocket)
- [Go memory model](https://go.dev/ref/mem) · [race detector](https://go.dev/doc/articles/race_detector)
- [PostgreSQL advisory locks](https://www.postgresql.org/docs/current/explicit-locking.html#ADVISORY-LOCKS)
- [Martin Kleppmann — How to do distributed locking (fencing tokens)](https://martin.kleppmann.com/2016/02/08/how-to-do-distributed-locking.html)
- [Redis replication — PSYNC, replication offset and backlog](https://redis.io/docs/latest/operate/oss_and_stack/management/replication/)
- `doc/reference/distributed_cache.md`, `doc/reference/codeheim_patterns.md`, `doc/purpose.md`
