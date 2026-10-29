# S4 Replica — Spec

**Status:** built (wave 3, 2026-10-29)
**Reference:** `advo_cache/replica/client.go:16-350` (Client: Connect, listen, monitorHeartbeat,
promoteToLeader, handleLeaderDisconnect), `advo_cache/ws/handler.go:453-632` (handleRegisterReplica,
sendFullState, replicate*, broadcastToReplicas, StartHeartbeat). Serves K6 (one-frame full state), K11
(no fencing), K12 (promoted replica has no password), D-012, D-013, D-014.

## Goal

Keep a hot replica of every sheet (entries, seq, bcrypt hash) so backendAdvo's reader fails over to it
when the leader dies, and fence the old leader with an epoch so the pair never settles on two leaders.

## Acceptance criteria

1. **Register.** A replica on `/replica` sends `register_replica{instance_id, started_at, epoch}`; the
   leader replies `{success:true, epoch}`, then one `snapshot_sheet` per sheet (`sheet`, `password_hash`,
   `project_id`, `data`, `expires_in_ms`, `seq`, `epoch`) and `snapshot_end{sheets, epoch}` (K6).
2. **Snapshot then stream.** Every op applied after the register reaches the replica after its sheet's
   snapshot and in seq order; keys set before the join and during the snapshot all arrive; the replica's
   seq per sheet equals the leader's.
3. **Stream.** `replicate_sheet`, `replicate`, `replicate_delete`, `replicate_delete_prefix`,
   `replicate_delete_suffix`, `replicate_expire` each carry `sheet`, `seq`, `epoch`; a full replica queue
   kicks that replica (it reconnects and resyncs). Heartbeat `{epoch, instance_id}` every
   `REDLITE_HEARTBEAT_INTERVAL`.
4. **Duplicate register.** A second `register_replica` on the same connection → `{success:false,
   error:"replica already registered"}`; a new connection with the same `instance_id` replaces the first
   (the old socket is closed).
5. **Replica refuses clients.** A replica that has not promoted answers `open` with `not leader` and
   reports `role: replica` at its leader's epoch on `/health`.
6. **REPLICA loop / promotion.** No frame from any leader for `REDLITE_HEARTBEAT_TIMEOUT` (measured from
   the last frame, across reconnect attempts, from boot or step-down) → the replica promotes with epoch
   `max(seen, stored)+1` (stored first when persistence is on) and serves; a reader with
   `BackupAddr` = replica fails over and keeps reading and writing. A replica that never reached a leader
   since boot promotes the same way (a lone replica serves; with persistence it hydrates first).
7. **K12.** A promoted replica requires the password the leader required (the bcrypt hash travels in
   `snapshot_sheet` / `replicate_sheet`).
8. **K11 fencing.** Only the peer probe fences: a leader steps down (Warn `Replica: stepping down`) when its
   peer's `/health` (`REDLITE_PEER_ADDRESS`, every heartbeat interval, 1s timeout) reports `leader` with a
   higher epoch. A client request or a registering replica that claims a higher epoch is refused
   (`stale epoch`) and only triggers an immediate probe; an unauthenticated frame never fences a
   leader. Step-down: stop accepting writes at once, kick every client and replica, follow the peer.
   After a healed partition exactly one node reports `leader` and a write through it reaches the other.
9. **Stale leader frames.** A replica that receives a frame with an epoch below its own logs Warn
   `Replica: stale leader frame` and drops the connection; a higher epoch is adopted. Frames are first
   decoded as `protocol.ReplicaOp` (type, sheet, epoch); no wire struct lives outside protocol/.
10. Every goroutine runs on a ctx; `Node.Run` is the one entry point; shutdown closes the hub first (no
    `/replica` handler enters and no connection registers after it, so `wg.Add` never races `wg.Wait`),
    kicks every replica socket and waits for their handlers (bounded by WriteTimeout + 1s).
11. **D-022 advisory election** (`REDLITE_ELECTION=advisory`, persistence required). `persist.Elector`
    holds `pg_try_advisory_lock(hashtext('redlite-leader'))` on one dedicated (hijacked) connection.
    A boot leader starts as a replica and promotes (epoch stored+1) only once it holds the lock; a replica
    promotes on timeout only if it gets the lock (else Warn `Replica: promotion deferred` and keeps
    following); a leader whose lock connection fails its ping steps down at once (Error
    `Persist: advisory lock lost`); a leader stepped down by the probe releases the lock. Epoch mode is
    unchanged.

## Surface

| Kind | Name | Package |
|---|---|---|
| func | `New(ctx, *sheet.Registry, Options) (*Node, error)` | internal/replica |
| type | `Options{Role, LeaderAddr, PeerAddr, InstanceID, HeartbeatInterval, HeartbeatTimeout, SendQueue, MaxFrameBytes, PingInterval, WriteTimeout, Store, KickClients, OnPromote}` | internal/replica |
| method | `IsLeader`, `Epoch`, `ObserveEpoch` (ws.Node); `Observe` (sheet.Observer); `ServeHTTP` (PathReplica); `Run(ctx) error`; `Role`, `Replicas` | internal/replica |
| method | `(*App).Node() *replica.Node`, `Store() *db.Store`, `Writer() *persist.Writer` | internal/app |
| func | harness `StartReplica`, `StartOn`, `Listen`, `Health`, `TryHealth` | testing/integration |

## Value-source table

| Value | Source (field / config / computed from) | Decided |
|---|---|---|
| role at boot | `REDLITE_ROLE` | ✅ config |
| leader URL | `REDLITE_LEADER_ADDRESS`; no path or `/ws` → `/replica` | ✅ config, D-010 |
| peer probe URL | `REDLITE_PEER_ADDRESS`, ws→http, wss→https, path `/health` | ✅ ASSUMPTIONS "Replica: fencing peer" |
| follow target after step-down | `REDLITE_PEER_ADDRESS`, else `REDLITE_LEADER_ADDRESS`; neither → fenced | ✅ D-012 (agent assumption, reported) |
| heartbeat interval / timeout | `REDLITE_HEARTBEAT_INTERVAL` / `REDLITE_HEARTBEAT_TIMEOUT` | ✅ config |
| replica queue / frame limit | `REDLITE_REPLICA_SEND_QUEUE` / `REDLITE_REPLICA_MAX_FRAME_BYTES` | ✅ config |
| boot epoch | leader: `cluster_state.epoch` (0 → 1, stored) or 1 without persistence; replica: 0 | ✅ D-012 |
| promotion epoch | `max(seen, stored) + 1`, stored via `RaiseEpoch` | ✅ D-012 |
| `instance_id`, `started_at` | `REDLITE_INSTANCE_ID` or generated; node boot time RFC3339 | ✅ app |
| `seq` | `sheet.Op.Seq` / `Snapshot.Seq` | ✅ op.go |
| `expires_in_ms` | `Entry.ExpiresAt − now`, min 1 when set; 0/absent = no expiry | ✅ R4 |
| `password_hash`, `project_id` | `Snapshot.PasswordHash/ProjectID`, `Op.PasswordHash/ProjectID` | ✅ D-013, K12 |
| probe timeout | 1s constant | ✅ brief |
| dial backoff | 50ms doubling to 5s, full jitter, forever | ✅ R1 shape (agent assumption, reported) |

## Failure behavior

| Failure | What the caller sees | What the system does |
|---|---|---|
| Leader socket down | reader: fails over to `BackupAddr` | replica reconnects with backoff; promotes after the timeout |
| Replica queue full | — | that replica is kicked, Warn `Replica: send queue full`; it resyncs |
| Partition | split-brain window: both accept writes | the old leader steps down on heal (probe / reader epoch); its partition writes are discarded (D-012) |
| Replica open | `not leader` | — |
| Persist unavailable at promotion | — | Error `Replica: epoch persist failed`; promotes on the in-memory epoch |
| Replica lag | stream is async: a read on the replica right after a leader write may miss | — |
| Stale leader | — | Warn `Replica: stale leader frame`, connection dropped |

## Reuse

`ws.Conn` (bounded queue + one writer + ping) for each replica socket; `sheet.Registry.Restore/Remove/
Sheets/KickAll`, `Sheet.Snapshot/Replace/ApplyReplicated`; `persist.LoadEpoch/RaiseEpoch`; `protocol`
frame types; `ws.Handler.KickAll` (through `Options.KickClients`).

## Decisions

D-010 (paths), D-012 (epoch fencing), D-013 (registry-owned sheets, hashes replicated), D-014 (queues).
ASSUMPTIONS: "Wire: replica stream", "Replica: locks", "Replica: client opens", "Replica: fencing peer".

## Wire compatibility

Additive: all frames are the ones in `protocol/messages.go` and `doc/protocol/wire.md` "Leader → replica".
New reply strings on `/replica` only (no reader reads them): `replica already registered`,
`instance_id required`, `register_replica required`.

## Out of scope

More than one replica; lock replication; a tie-break for two leaders at the same epoch (cannot arise in
a pair: only a promotion raises the epoch).

## Tests

| Criterion | Test file | Kind |
|---|---|---|
| 1, 4 | `testing/integration/replica_test.go` TestDuplicateRegisterReplica | integration |
| 2, 3 | `testing/integration/replica_test.go` TestSnapshotThenStream, TestReplicaLoop | integration |
| 5 | `testing/integration/replica_test.go` TestReplicaRefusesOpenBeforePromotion | integration |
| 6, 7 | `testing/integration/replica_test.go` TestReplicaLoop | integration |
| 8 | `testing/integration/replica_test.go` TestFencingStepDown (probe), TestFencingByReaderEpoch, TestDuplicateRegisterReplica (claims refused, never fence) | integration |
| 6 (persist) | `testing/integration/persist_test.go` TestPersistEpochAcrossPromotion | integration (DB) |
| 9, 10 | NO TEST: `internal/replica/follower.go` stale-frame drop (needs a fake leader); shutdown covered by every test's Stop under -race | — |
| 11 | `testing/integration/control_test.go` TestAdvisoryElection | integration (DB, skips) |
