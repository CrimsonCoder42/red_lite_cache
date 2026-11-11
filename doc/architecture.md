# Architecture

_Verified against the code on 2026-10-29 (bootstrap). Every file:line below was read, not recalled. A
line that no longer matches is a bug in this file._

## The cache layer, in one paragraph

Each consumer process embeds `reader/`: a local `map[string]{json.RawMessage, expiresAt}` it reads in
O(1) with no network. The leader (`cmd/redlite`) holds the authoritative copy per **sheet** (tenant),
applies every write under that sheet's mutex, and pushes the result to every other collaborator
before it can serve a stale read. A hot-standby replica follows the leader over the gRPC `Replica/Follow` stream and
promotes itself, fenced by an epoch, when the leader goes quiet. PostgreSQL, when enabled, is the
control plane and the durable copy — never on the read or write path (D-019).

## Processes

```
 consumer ──────────────┐            ┌──────────────── leader (cmd/redlite, internal/app) ─────────────────┐
 reader.Reader ─gRPC────┼──────────▶ │ internal/rpc Cache.Session → session.route → sheet.Apply (sheet mutex)│
 Cache.Session :8095    │            │   per-stream rpc.Conn: bounded send queue + 1 writer (keepalive)     │
 reconnect forever,     │            │ internal/sheet Registry → Sheet{cache.Store, collaborators, seq,     │
 queued deletes, epoch  │            │                 sweep log (R6)} ── Observers ─┬─▶ replica hub        │
                        │            │ internal/lock  Manager (owner = conn id, TTL) ├─▶ persist.Writer ──▶ PG│
                        │            │ admin port :8097  internal/api /admin/*, GET /health {role, epoch}   │
                        │            │ grpc.health.v1 on :8095; Replica/Probe = the peer fence (D-049)      │
                        │            └───────────────────────────┬──────────────────────────────────────────┘
                        │      Replica/Follow: register → snapshot_sheet chunks×N → snapshot_end → stream
                        │            ┌───────────────────────────▼─────────────── replica ──────────────────┐
                        └─(backup)─▶ │ internal/replica follower: Restore/Replace/ApplyReplicated            │
                                     │ refuses `open` (not leader) until it promotes (epoch+1)               │
                                     └───────────────────────────────────────────────────────────────────────┘
```

## Startup order — the docker-compose development stack (D-047)

`docker compose up -d` (`docker-compose.yml`): **postgres** (healthy = `pg_isready`; `docker/postgres/`
creates `redlite_test` on a fresh volume) → **leader** (`depends_on` postgres healthy; migrates its schema
at boot, `db/store.go:114` `Migrate`, hydrates, then serves; healthy = `GET /health` on the admin port 8097) → **bootstrap**
(`depends_on` leader healthy; one-shot `docker/bootstrap/bootstrap.sh`: admin API → project + sheet →
`./.redlite/dev-project.env`) and, with `--profile cluster`, **replica** (`depends_on` leader healthy).
Host ports bind to 127.0.0.1: leader 8095 (gRPC) + 8097 (admin), replica 8096 + 8098, postgres 5434.

## Write path (set, delete, delete_prefix, delete_suffix)

1. `rpcwire.ClientRequest` turns the `ClientMsg` into a `protocol.Request`; the `internal/rpc` router
   gates it: open sheet, leader role, epoch (`internal/rpc/router.go`).
2. `sheet.Apply` takes the sheet mutex, checks `base_seq` against the sweep log (R6) and `MaxKeys`,
   applies to `cache.Store`, bumps `seq`, encodes ONE broadcast frame, enqueues it to every
   collaborator except the sender (a full queue kicks that collaborator, D-014), enqueues the
   sender's reply, and calls every Observer — all before the mutex is released (`internal/sheet/sheet.go:262`).
3. Observers enqueue without blocking: the replica hub (one frame to every replica) and the persist
   writer (one op to its bounded channel; full → drop + Warn).
4. Each stream's writer goroutine drains its queue with a write timeout, converting each frame to
   `ServerMsg`s (`rpcwire.ServerMsgs`; a sheet state as sorted ≤ 1 MiB chunks). Order on every stream
   is the leader's apply order (K10). A send held by HTTP/2 flow control never holds the writer past
   a kick: the stream is aborted (`StreamTransport.Write`).

## Read path

`reader.Get(key, &T)` reads the local map under an RWMutex: a miss while disconnected (R3), after the
local expiry (R4), or for `"__NULL__"`; a hit decodes the raw JSON once into the caller's type (R8). It
never touches the network or Postgres.

## Goroutines per process

| Goroutine | Started at | Exits on |
|---|---|---|
| HTTP server (accept + one per request, net/http) | `internal/app/app.go:137` (`srv.Serve`) | ctx → `srv.Shutdown` |
| cache janitor — ONE per process, every sheet, leader only | `internal/app/app.go:145` → `sheet.RunJanitor` | ctx |
| lock cleaner | `internal/app/app.go:146` → `lock.RunCleaner` | ctx |
| shutdown watcher (CloseAll, Shutdown with deadline) | `internal/app/app.go:154` | ctx |
| replica node (leader: heartbeats; replica: follow + promote) | `internal/app/wire_replica.go:141` → `replica.Node.Run` (`internal/replica/node.go:208`) | ctx |
| peer probe (leader with `REDLITE_PEER_ADDRESS`) | `internal/replica/leader.go:256` | step-down / ctx |
| heartbeat monitor (replica following a leader) | `internal/replica/follower.go:56` | connection end / ctx |
| persist writer — one | `internal/app/wire_replica.go:143` → `persist.Writer.Run` | ctx, after the final flush |
| persist TTL cleaner | `internal/app/wire_replica.go:144` → `persist.RunCleaner` | ctx |
| per client/replica stream: read loop | the gRPC handler goroutine (`rpc.Conn.Run`) | stream end / ctx |
| per stream: receive pump | `internal/rpc/grpc.go` `NewStreamTransport` | stream ctx / Abort |
| per stream: sender | `internal/rpc/grpc.go` `NewStreamTransport` | stream ctx / Abort |
| per stream: writer | `internal/rpc/conn.go` `writeLoop` | kick / close / ctx |
| gRPC transport (per connection) | grpc-go (reader, loopy writer, keepalive) | connection close / GracefulStop |
| replica hub shutdown waiter | `internal/replica/node.go:316` | all replica handlers returned (bounded by ShutdownTimeout) |
| reader: connect/reconnect loop (consumer process) | `reader/reader.go:154` | Close / Config.Context |
| reader: read loop per session | `reader/conn.go` `readLoop` | stream end |
| reader: receive pump per session | `reader/link.go` `recvLoop` | link close |
| reader: local expiry pass, every 1 s on the reader's clock (D-045) | `reader/reader.go:184` → `expireLoop` (`reader/reader.go:210`) | Close / Config.Context |

No goroutine per entry, per sheet, or per request beyond grpc-go's and net/http's own. Every one takes a ctx.

## Package dependency rule

`protocol` (with the generated `protocol/redlitev1` and `protocol/rpcwire`) ← `internal/cache` ←
`internal/sheet` ← `internal/rpc`, `internal/persist`, `internal/replica`, `internal/api` ←
`internal/app` ← `cmd/redlite`. `internal/replica` also imports `internal/rpc` (for `rpc.Conn`,
`rpc.Transport`) and `db`. `reader` imports `protocol`, `protocol/redlitev1`, `protocol/rpcwire`,
`internal/cache` (the shared `Groups` index and `Wheel`, D-044/D-045) and `google.golang.org/grpc`
(`go list -deps ./reader`). No package imports `cmd`.

## Bounds

Every structure that can accumulate, its cap, how it is freed, the Stats field that measures it (D-040) and the
leak test that proves it returns to baseline (`testing/leaks`, D-038; numbers in `doc/leaks_2026-10-29/README.md`).

| Structure | Cap (config var, default) | Freed on | Stats field | Test |
|---|---|---|---|---|
| sheet store entries + ordered index (forward and reversed B+tree nodes, `cache.Groups`, D-044) + expiry wheel items (`cache.Wheel`, D-045) | `REDLITE_CACHE_MAX_KEYS_PER_SHEET` (0 = unbounded) + TTL; wheel ≤ 2 × live items after a Set | Expire (walks the wheel's due seconds), Delete*, Replace; nodes merge on underflow; map, index and wheel replaced when the last key goes | `sheet.Stats.Store{Entries, ForwardNodes, ReverseNodes, WheelItems, WheelSlots}` | L1, L2, L12; `cache` `TestWheelCompaction` |
| sweep log (per sheet) | `REDLITE_SWEEP_LOG_MAX` 4 096, `REDLITE_SWEEP_LOG_WINDOW` 30 s | every write, every janitor pass (replica too) | `SweepLogEntries` | L5 |
| sheets | none — never evicted (D-013, D-042) | Drop (revoke), Remove (replica resync) | `Sheets` | L11, L12 |
| collaborators | per connection | Leave, full queue kick, drop | `Collaborators` | L6, L7, L11 |
| per-connection send queue | `REDLITE_CONN_SEND_QUEUE` 256 (`REDLITE_REPLICA_SEND_QUEUE` 4 096) | connection close; full = kick | `rpc.Conn.Queued`, `App.QueuedFrames` | L6 |
| goroutines per connection | 3 (read, writer, ping) | connection close | `HealthStats.Goroutines` | L6, L7, L9 |
| sessions | per connection | close, kick, suspend, revoke | `session.Registry.Len` | L6, L7 |
| locks + owner index | per connection × key, `REDLITE_LOCK_TTL` | owner disconnect, cleaner tick | `lock.Stats{Held, Owners}` | L9 |
| tenant revoked names / dead key ids | `REDLITE_TENANT_MEMORY_MAX` 10 000 each, oldest dropped, one Warn | restart | `tenant.Stats{RevokedNames, DeadKeys, *Dropped}` | L8 |
| persist queue / parked / held | `REDLITE_PERSIST_QUEUE_SIZE` 10 000; parked ≤ sheets; held ≤ one batch | flush | `persist.Stats{Queued, Parked, Held, Dropped}` | L10 |
| reader local map (+ the same `cache.Groups` and `cache.Wheel` as the server) | `reader.Config.MaxEntries` 100 000 | invalidate, eviction, the local expiry pass every 1 s (D-045), Close (map, index and wheel replaced when emptied) | `reader.Stats{Entries, ForwardNodes, ReverseNodes, WheelItems}` | L3, L2, L12, L13; `reader` `TestReaderDropsExpiredOffline` |
| reader pending deletes | `reader.Config.MaxPendingDeletes` 10 000 → collapses to `delete_prefix ""` | replay on reconnect, Close | `PendingDeletes` | L4 |
| reader miss records | 65 536 keys | dead records swept once per MissWindow; Close | `MissRecords` | L3, `reader` `TestMissRecordsSweptAfterWindow` |

Known gap: with persistence off a replica keeps a revoked project's sheets until it resyncs (F-1, `doc/ASSUMPTIONS.md`).

## Failure behaviour

| Failure | What happens |
|---|---|
| Reader link drops | `IsConnected` false, reads miss, deletes queue; reconnect forever (leader ↔ backup); queued deletes replayed and acked before reads resume |
| Slow collaborator | its send queue fills → that connection is closed; the sheet never waits (`TestEdgeSlowConsumerKicked`) |
| Leader dies | replica promotes after `REDLITE_HEARTBEAT_TIMEOUT` at epoch+1; readers fail over (`TestReplicaLoop`) |
| Partition heals with two leaders | the lower epoch steps down on the peer probe, a registering replica or a reader write with a higher epoch (`TestFencingStepDown`, `TestFencingByReaderEpoch`) |
| Postgres down | memory keeps serving; flushes fail, retry once, then drop with an Error log |
| Restart with persistence | sheets (with hashes) and live entries hydrated before the listener starts |
