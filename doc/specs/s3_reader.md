# S3 Reader — Spec

**Status:** built (unit-tested against an in-package fake server; integration against the real server lands in wave 3, `testing/integration/{disconnect,race}_test.go`)
**Reference:** `advo_cache/reader/reader.go:1-604` (the v1 reader this replaces); consumer workarounds `backendAdvo/cache/store.go:99-226, 330-520`

## Goal

The embeddable client backendAdvo imports: O(1) local reads of one sheet kept in sync over a WebSocket, acknowledged
writes, and a link that heals itself — so the consumer can delete its DROPPED SWEEPS queue, `linkReady` gate, second
sweep and double JSON pass (`doc/purpose.md` "What success looks like").

## Acceptance criteria

1. **R1** — The reader reconnects forever after the server is down, far past v1's 3+3+3 attempts, alternating
   LeaderAddr/BackupAddr with backoff ReconnectMin doubling to ReconnectMax, full jitter, reset after a connection that
   lasted min(ReconnectMax, 10s).
2. **R2** — `Delete`/`DeletePrefix`/`DeleteSuffix` while disconnected apply locally, return nil, and are replayed in
   issue order after reconnect; the replay is acked **before** `IsConnected` is true and before `Get` serves; the
   reconnect's `sheet_state` holding a deleted key does not resurrect it. Queue overflow (MaxPendingDeletes) collapses
   to one `delete_prefix ""` (D-005). A delete while connected is sent and acked; an unacked one (write failure,
   WriteTimeout, link lost) is requeued and the link is reset.
3. **R3** — While disconnected `Get`, `GetRaw`, `Has`, `IsNull` miss although the local map still holds the key.
4. **R4** — A key misses after the `expires_in_ms` it arrived with (sheet_state, replicate, set ack), measured from
   receipt; the local map never exceeds MaxEntries (a new key on a full map evicts an expired entry, else an arbitrary one).
5. **R6** — A set carries `base_seq`: the applied seq at this reader's OLDEST live miss of the key, else the current
   applied seq (a write-through with no read). Per key the reader keeps the earliest miss of each `MissWindow/2` slice;
   an entry is live for `MissWindow` (default 30s, should match the server's `REDLITE_SWEEP_LOG_WINDOW`). A later miss
   never raises a kept entry, an expired entry is dropped, and a set (stored or refused) never consumes one, so every
   read-through of the key on one reader whose miss-to-set time is under MissWindow/2 sends a base at or before its own
   miss. Consequence: after a covering write, read-through sets of that key are refused for up to MissWindow, never
   stale. After a reconnect every recorded seq becomes 0 (times kept). The record is bounded (65 536 keys); on
   overflow keys with no live entry go first, then the map is dropped and keys without one send the lowest dropped
   seq while it is live.
6. **R8** — `Get(key, &target)` decodes the stored bytes into the caller's type per call; mutating the result does not
   change the next `Get`. `GetRaw` returns the v1 shape (`map[string]any`, …). The `"__NULL__"` placeholder is a `Get`
   miss and `IsNull` true.
7. **R9** — `SetWithTTL` waits for the ack. `stale write` → `ErrStale`; any other server error → a wrapped error; no ack
   within WriteTimeout/ctx → `ErrTimeout`; not connected → `ErrNotConnected`. On every failure nothing is stored. On
   success the value is stored by the read loop (server order vs broadcasts) with the ack's `expires_in_ms`.
8. **R10** — `New` against a dead address returns a non-nil reader within ConnectTimeout, `IsConnected` false, and it
   connects when the server appears. `New` returns `(nil, ErrAuthFailed)` for a refused password, without waiting
   ConnectTimeout, and never logs the password.
9. **D-012** — The reader remembers the highest epoch from open replies; a server answering with a lower one is refused
   and the reader moves to the other address. Every write and open carries `epoch`; `version` rides on `open`.
10. **Lifecycle** — `Close` is idempotent, waits for every goroutine (none leak), and afterwards writes return
    `ErrClosed` and reads miss. Cancelling `Config.Context` is `Close`.
11. **Dependencies** — `reader/` imports only the standard library, `protocol/` and `github.com/coder/websocket`; no
    globals, no `init`, no environment reads.

## Surface

| Kind | Name | Package |
|---|---|---|
| var | `ErrNotConnected`, `ErrClosed`, `ErrAuthFailed`, `ErrStale`, `ErrTimeout` | reader |
| const | `DefaultConnectTimeout`, `DefaultWriteTimeout`, `DefaultMaxEntries`, `DefaultMaxPendingDeletes`, `DefaultReconnectMin`, `DefaultReconnectMax`, `DefaultMissWindow` | reader |
| type | `Config{LeaderAddr, BackupAddr, Sheet, Password, Context, Logger, ConnectTimeout, WriteTimeout, MaxEntries, MaxPendingDeletes, ReconnectMin, ReconnectMax, MissWindow}` | reader |
| func | `New(Config) (*Reader, error)` | reader |
| method | `Get(key, target) bool`, `GetRaw(key) (any, bool)`, `Has`, `IsNull`, `Set`, `SetWithTTL`, `Delete`, `DeletePrefix`, `DeleteSuffix`, `IsConnected`, `Close` | reader |
| frames sent | `open`, `set`, `delete`, `delete_prefix`, `delete_suffix` | protocol.Request |
| frames read | open/write replies (`protocol.Response`, matched by `id`), `sheet_state`, `replicate`, `invalidate` | protocol |

## Value-source table

| Value | Source (field / config / computed from) | Decided |
|---|---|---|
| `open.sheet`, `open.password` | `Config.Sheet`, `Config.Password` | ✅ |
| `open.version` | `protocol.Version` (D-010) | ✅ |
| `epoch` on open and every write | highest `Response.Epoch` seen on an open reply (D-012); 0 omitted | ✅ |
| `id` on every request | per-reader counter, `atomic.Uint64` | ✅ |
| `set.value` | `json.Marshal(value)` once, caller's value | ✅ |
| `set.ttl` | `ttlMinutes` argument (MINUTES, v1); <=0 sent as absent = server default | ✅ |
| `set.base_seq` | oldest live (< MissWindow) miss entry for the key (one per MissWindow/2 slice), else live fallback after overflow, else applied seq (R6, lead decision wave 3) | ✅ |
| applied seq | `sheet_state.seq` (set, not max — a new server's numbering), then max of every `replicate`/`invalidate`/ack `seq` | ✅ |
| local expiry | receipt time + `expires_in_ms` (sheet_state map / replicate / set ack), 0 = none (D-008) | ✅ |
| `delete*.key/prefix/suffix` | the caller's argument | ✅ |
| overflow op | `delete_prefix ""` (D-005) | ✅ |
| backoff | ReconnectMin..ReconnectMax, full jitter (`math/rand/v2`), D-004 | ✅ |

## Failure behavior

| Failure | What the caller sees | What the system does |
|---|---|---|
| Socket down | reads miss; `IsConnected` false; `Set` → `ErrNotConnected`; `Delete*` → nil | reconnects forever; deletes applied locally and queued |
| Sheet not open / open refused | same as socket down; `New` → `ErrAuthFailed` only for `invalid password` | that address counts as failed; alternate |
| Lock held | n/a (locks not in the reader surface; 0 call sites in backendAdvo) | — |
| Persist unavailable | n/a (server side) | — |
| Replica lag / stale leader | a replica's `not leader`/`stale epoch` reply: `Set` → `ErrNotConnected`, delete requeued | link reset, fail over; lower epoch on open refused |
| No ack in WriteTimeout | `Set` → `ErrTimeout`; `Delete*` → nil | delete: link reset, delete requeued and replayed |
| Queue over MaxPendingDeletes | `Delete*` → nil | queue collapses to one `delete_prefix ""`; Warn |

## Reuse

`protocol.Request/Response/SheetState/Replicate/Invalidate/Envelope`, `protocol.NullValue`, `protocol.ErrMsg*`,
`protocol.Version`, `protocol.Ptr`. Shapes from `advo_cache/reader/reader.go` (origin comments at each:
local map 107,157; New 121-148; Get 157-159; SetWithTTL 213-237; Delete* 242-321; connectTo 376-441; loadState
443-454; handleMessage 484-524; handleDisconnect 530-584). WebSocket: `coder/websocket` (D-011) — it serialises
writes, so the reader's `wmu` exists only to keep request ids in wire order for v1-server reply matching.

## Decisions

D-002 (map + RWMutex), D-004 (reconnect forever), D-005 (queue + replay), D-008 (TTL + bound), D-010 (version on
open), D-011 (coder/websocket), D-012 (epoch).

## Wire compatibility

Additive only; nothing in the v1 compatibility list changes. What this reader sends that a **v1 server ignores**:
`id`, `version`, `epoch` (open and writes), `base_seq` (set). Against a v1 server: replies carry no `id`, so the reader
matches a reply without one to its oldest pending request (a v1 server answers in order); acks carry no `seq` or
`expires_in_ms`, so values have no local expiry and base_seq stays at the sheet_state's 0. Fields it reads that a v1
server never sends (`seq`, `epoch`, `expires_in_ms`, `keys`) are optional and default to "none". A v1 `invalidate`
with `prefix:""` now clears the local sheet, matching what the server did.

## Out of scope

`lock`/`unlock`, `set_null`, `dump`, `get` (reads are local), `GetString`/`GetMap`/`CurrentServer`/`CacheSize` (v1
helpers with 0 call sites in backendAdvo), singleflight in the reader (the consumer's loader owns it).

## Tests

All in `reader/reader_test.go` (line 1 `// Covers: reader`), against `reader/fakeserver_test.go`.

| Criterion | Test | Kind |
|---|---|---|
| 1 R1 | `TestR1ReconnectForever` | unit (fake server) |
| 2 R2 | `TestR2DeletesQueuedAndReplayed`, `TestR2QueueOverflowCollapses`, `TestDeleteConnectedAcked` | unit |
| 3 R3 | `TestR3DisconnectedReadsMiss` | unit |
| 4 R4 | `TestR4ExpiryAndBound` | unit |
| 5 R6 | `TestR6BaseSeq`, `TestR6MissWindowReplaces` (unit); `TestRaceLoop` (integration) | unit + integration |
| 6 R8 | `TestR8TypedIsolatedGet` | unit |
| 7 R9 | `TestR9AckedSet` | unit |
| 8 R10 | `TestR10NewWhileDown`, `TestAuthFailed` | unit |
| 9 D-012 | `TestEpochRefusedFailsOver` | unit |
| 10 lifecycle | `TestCloseIdempotentNoLeak`, `TestContextCancelCloses`, `TestBroadcastsApplied` | unit |
| 11 deps | `go list -deps ./reader` (report) | check |
