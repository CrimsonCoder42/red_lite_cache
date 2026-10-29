# Architecture

_Verified against the code on 2026-10-29 (bootstrap). Every file:line below was read, not recalled. A
line that no longer matches is a bug in this file._

## The cache layer, in one paragraph

Each consumer process embeds `reader/`: a local `map[string]{json.RawMessage, expiresAt}` it reads in
O(1) with no network. The leader (`cmd/redlite`) holds the authoritative copy per **sheet** (tenant),
applies every write under that sheet's mutex, and pushes the result to every other collaborator
before it can serve a stale read. A hot-standby replica follows the leader over `/replica` and
promotes itself, fenced by an epoch, when the leader goes quiet. PostgreSQL, when enabled, is the
control plane and the durable copy — never on the read or write path (D-019).

## Processes

```
 consumer ──────────────┐            ┌──────────────── leader (cmd/redlite, internal/app) ─────────────────┐
 reader.Reader ──ws /ws─┼──────────▶ │ internal/ws  Handler → session.route → sheet.Apply (per-sheet mutex) │
 (local map, O(1) Get)  │            │   per-conn ws.Conn: bounded send queue + 1 writer + ping             │
 reconnect forever,     │            │ internal/sheet Registry → Sheet{cache.Store, collaborators, seq,     │
 queued deletes, epoch  │            │                 sweep log (R6)} ── Observers ─┬─▶ replica hub        │
                        │            │ internal/lock  Manager (owner = conn id, TTL) ├─▶ persist.Writer ──▶ PG│
                        │            │ internal/api   /admin/* (projects, keys, export)                      │
                        │            │ GET /health {role, epoch}                                            │
                        │            └───────────────────────────┬──────────────────────────────────────────┘
                        │                     /replica: register → snapshot_sheet×N → snapshot_end → stream
                        │            ┌───────────────────────────▼─────────────── replica ──────────────────┐
                        └─(backup)─▶ │ internal/replica follower: Restore/Replace/ApplyReplicated            │
                                     │ refuses `open` (not leader) until it promotes (epoch+1)               │
                                     └───────────────────────────────────────────────────────────────────────┘
```

## Write path (set, delete, delete_prefix, delete_suffix)

1. `internal/ws` router gates the request: open sheet, leader role, epoch (`internal/ws/router.go`).
2. `sheet.Apply` takes the sheet mutex, checks `base_seq` against the sweep log (R6) and `MaxKeys`,
   applies to `cache.Store`, bumps `seq`, encodes ONE broadcast frame, enqueues it to every
   collaborator except the sender (a full queue kicks that collaborator, D-014), enqueues the
   sender's reply, and calls every Observer — all before the mutex is released (`internal/sheet/sheet.go:262`).
3. Observers enqueue without blocking: the replica hub (one frame to every replica) and the persist
   writer (one op to its bounded channel; full → drop + Warn).
4. Each connection's writer goroutine drains its queue with a write timeout. Order on every socket is
   the leader's apply order (K10).

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
| per WS connection: read loop | the HTTP request goroutine (`ws.Conn.Run`) | socket close / ctx |
| per WS connection: writer | `internal/ws/conn.go:165` | kick / close / ctx |
| per WS connection: ping | `internal/ws/conn.go:167` | close / ctx |
| replica hub shutdown waiter | `internal/replica/node.go:316` | all replica handlers returned (bounded by ShutdownTimeout) |
| reader: connect/reconnect loop (consumer process) | `reader/reader.go:154` | Close / Config.Context |
| reader: read loop per session | `reader/conn.go:185` | socket close |

No goroutine per entry, per sheet, or per request beyond net/http's own. Every one takes a ctx.

## Package dependency rule

`protocol` ← `internal/cache` ← `internal/sheet` ← `internal/ws`, `internal/persist`, `internal/replica`,
`internal/api` ← `internal/app` ← `cmd/redlite`. `internal/replica` also imports `internal/ws` (for
`ws.Conn`) and `db`. `reader` imports only `protocol` and `github.com/coder/websocket`
(`go list -deps ./reader`). No package imports `cmd`.

## Failure behaviour

| Failure | What happens |
|---|---|
| Reader link drops | `IsConnected` false, reads miss, deletes queue; reconnect forever (leader ↔ backup); queued deletes replayed and acked before reads resume |
| Slow collaborator | its send queue fills → that connection is closed; the sheet never waits (`TestEdgeSlowConsumerKicked`) |
| Leader dies | replica promotes after `REDLITE_HEARTBEAT_TIMEOUT` at epoch+1; readers fail over (`TestReplicaLoop`) |
| Partition heals with two leaders | the lower epoch steps down on the peer probe, a registering replica or a reader write with a higher epoch (`TestFencingStepDown`, `TestFencingByReaderEpoch`) |
| Postgres down | memory keeps serving; flushes fail, retry once, then drop with an Error log |
| Restart with persistence | sheets (with hashes) and live entries hydrated before the listener starts |
