# S10 gRPC wire — Spec

Status: **built and WebSocket removed** 2026-11-11 (doc/grpc_build_2026-11-11). Decisions: D-048
(the wire is gRPC; JSON over WebSocket retired), D-049 (the admin port), D-050 (status mapping).

## Goal

Replace JSON-over-WebSocket with gRPC. The `.proto` files are the contract; any language with a gRPC
generator can be a client. Behaviour does not change: the same router, admission, sessions, send
queue and sheet code that served WebSocket serve gRPC (both wires ran every suite before WebSocket
was removed).

## Acceptance criteria

1. `proto/redlite/v1/cache.proto` and `replica.proto` cover every frame of the v1 table (doc/protocol/wire.md), except `dump` (moved to `/admin`, answered "unknown action") and `register_replica` (its own service).
2. One bidi stream per session (`Cache.Session`) carries requests, replies and broadcasts in one order (R2, R6). Every message names its sheet.
3. A sheet state (on open, on resync) and a replica snapshot arrive as sorted chunks of at most 1 MiB with a final `last` marker; no message reaches 4 MB at any size of sheet. The reader bulk-builds from the assembled chunks (S9 step 4).
4. Each stream has one writer behind the bounded send queue. A full queue ends that stream `RESOURCE_EXHAUSTED` / `SLOW_CONSUMER`; a broadcast never waits on a peer's flow control.
5. The client port (`:8095` leader, `:8096` replica) serves `redlite.v1.Cache`, `redlite.v1.Replica` and `grpc.health.v1.Health`. Admin REST and `GET /health` stay HTTP on `REDLITE_ADMIN_PORT` (`:8097`).
6. Admission is unchanged and cannot be bypassed: the API key travels as `authorization: Bearer <key>` call metadata (v1 password admission still works); `Replica/Follow` and `Replica/Probe` need the cluster token as call credentials; every other unary call except health is refused.
7. Keepalive (server time = `REDLITE_PING_INTERVAL`, timeout = `REDLITE_WRITE_TIMEOUT`, enforcement min 5 s), graceful stop bounded by `REDLITE_SHUTDOWN_TIMEOUT`.
8. Reader and v1compat public APIs unchanged; `Config.LeaderAddr` takes `host:port`; a `ws://` address is refused by `New` with an error naming D-048.
9. Every suite passed on both wires (`REDLITE_TEST_TRANSPORT=ws|grpc`, since removed) before WebSocket was removed: grpc 412 | 407 | 1 | 4, ws 412 | 409 | 1 | 2; after removal 404 | 401 | 1 | 2 (the one failure is the pre-existing jsonb `TestPersistLoop`).

## Surface

| Old (WebSocket) | New (gRPC) |
|---|---|
| `GET /ws` upgrade, JSON text frames | `redlite.v1.Cache/Session` bidi stream, `ClientMsg` / `ServerMsg` |
| `GET /replica` upgrade + Bearer header | `redlite.v1.Replica/Follow` bidi stream + Bearer metadata |
| peer `GET /health` with token (fencing) | `redlite.v1.Replica/Probe` with token |
| `GET /health`, `/admin/*` on the client port | same paths on `REDLITE_ADMIN_PORT` |
| WebSocket close code | gRPC status + `redlite.v1.ErrorDetail{reason, message, close_code}` |

## Status mapping (D-050)

| close code / reason | status | reason | reader |
|---|---|---|---|
| 4001 suspended | PERMISSION_DENIED | SUSPENDED | terminal |
| 4002 key revoked | UNAUTHENTICATED | KEY_REVOKED | terminal |
| 4003 kicked | ABORTED | KICKED | terminal |
| 1008 invalid password / not authorized | UNAUTHENTICATED | INVALID_PASSWORD / NOT_AUTHORIZED | ErrAuthFailed on New, retry later |
| 1008 stale epoch | FAILED_PRECONDITION | STALE_EPOCH | fail over |
| 1009 | RESOURCE_EXHAUSTED | MESSAGE_TOO_LARGE | reconnect |
| send queue full | RESOURCE_EXHAUSTED | SLOW_CONSUMER | reconnect, resync |
| not leader | FAILED_PRECONDITION | NOT_LEADER | fail over |
| anything else (1001, shutdown) | UNAVAILABLE | GOING_AWAY | reconnect |

## Value-source table

| Value | Source |
|---|---|
| client port | `REDLITE_PORT` |
| admin port | `REDLITE_ADMIN_PORT` (default `:8097`) |
| peer admin base URL | `REDLITE_PEER_ADMIN_ADDRESS` (default: the peer's host on this node's admin port) |
| receive limit | `REDLITE_MAX_FRAME_BYTES` (client, default 4 MiB = `rpcwire.DefaultMaxMessageBytes`), `REDLITE_REPLICA_MAX_FRAME_BYTES` (replica); the reader receives up to 64 MiB and refuses a set over 4 MiB (`ErrTooLarge`) |
| chunk size | `rpcwire.DefaultChunkBytes` = 1 MiB (constant) |

## Failure behavior

A malformed `authorization` ends the call UNAUTHENTICATED before any session exists. An empty or
unknown `ClientMsg` body is answered "unknown action" and the stream stays up (forward compatible).
A peer that stops reading: its queue fills, its stream ends SLOW_CONSUMER; the writer's acks are
not delayed. A send held by flow control is released when the RPC ends (Abort).

## Reuse

`internal/rpc` holds the core (Conn, router, admission, Handler) behind a `Transport` interface; the
gRPC `StreamTransport` is its one implementation (the WebSocket adapter was the other, now removed). `protocol/rpcwire` converts the JSON frames
of package `protocol` to and from proto at the edge, for the server, the reader and the replica.

## Wire compatibility

The JSON v1 wire is removed (D-048): `internal/ws` is in `_delete/`, `github.com/coder/websocket` is out
of go.mod. v1 readers (advo_cache) cannot connect; consumers use `reader` or `reader/v1compat`
(same APIs) with a `host:port` address (consumer/backendadvo_migration.md).

## Out of scope

TLS (terminated in front, as before), s8 project sign-in, the jsonb persist fix, making the sheet
emit proto directly (needs internal/sheet changes). Performance follow-up: per-op latency is ~1.5×
WebSocket and a 30-reader broadcast ~1.9×; the profile puts the cost in per-message HTTP/2 writes and
flushes (66 % syscalls), not in the JSON↔proto conversion — batching/coalescing writes is the lever
(doc/grpc_build_2026-11-11/out/bench_ws_vs_grpc.txt).

## Tests

`internal/rpc/grpc_test.go` (bufconn: every message, every status, slow reader, 100k-key state),
`internal/rpc/conn_test.go` (Conn over an in-memory transport), `reader/fakeserver_test.go` +
`reader/grpcfake_test.go` (fake gRPC server), `testing/blackbox` (address + .proto only),
`testing/wirebench` (benchmarks),
`protocol/redlitev1/stale_test.go` (generated code current), and every harness suite on both wires.
