# Wire protocol — gRPC (redlite.v1)

_Contract: `proto/redlite/v1/cache.proto` and `replica.proto` are the source of truth; generated Go
code is in `protocol/redlitev1` (`make proto`; `protocol/redlitev1/stale_test.go` fails when it is
stale). This file describes them. The JSON-over-WebSocket wire (v1, `/ws`, `/replica`) was retired
on 2026-11-11 (D-048); its frame table is kept at the end for the record._

## Ports

| port | serves |
|---|---|
| `REDLITE_PORT` (`:8095` leader, `:8096` replica) | gRPC: `redlite.v1.Cache`, `redlite.v1.Replica`, `grpc.health.v1.Health` |
| `REDLITE_ADMIN_PORT` (`:8097`) | HTTP: `GET /health`, `/admin/*` (doc/specs/s6_api.md) |

Plaintext gRPC (h2c); TLS is terminated in front, as before.

## Cache.Session — one bidi stream per client session

`rpc Session(stream ClientMsg) returns (stream ServerMsg)`. Requests, their replies and the
sheet's broadcasts travel on the one stream **in one order** (R2, R6): a reply is never overtaken by
a broadcast of a later write, nor the reverse. Every message names its sheet.

**Admission.** API key mode: call metadata `authorization: Bearer <api key>` (D-024); a malformed
`authorization` ends the call `UNAUTHENTICATED` before any session exists. Password mode (v1
admission): no metadata, the password in `Open`.

**ClientMsg** `{id, epoch, oneof body}` — `id` is echoed on the reply's ServerMsg; `epoch` is the
highest leader epoch the client has seen (a server with a lower epoch refuses `stale epoch`, D-012).

| body | fields | Reply |
|---|---|---|
| `open` | `sheet`, `password`, `version` | `{success, sheet, protocol_version, epoch, seq}`, then the sheet's state as `StateChunk`s. Refusals as before: `invalid password` / `not authorized` (then the stream ends `UNAUTHENTICATED`), `suspended` (ends 4001), `sheet not found`, `not leader` |
| `get` | `sheet`, `key` | `{success, key, value}` or `key not found` |
| `set` | `sheet`, `key`, `value` (bytes: the JSON value, stored as given), `ttl_minutes`, `base_seq?` (R6) | `{success, key, seq, expires_in_ms}`; broadcasts `Replicate`. Refused: `stale write`, `sheet full` |
| `set_null` | `sheet`, `key`, `ttl_minutes` | as `set`; stores `"__NULL__"` |
| `delete` | `sheet`, `key` | `{success, key, deleted, seq}`; broadcasts `Invalidate{key}` |
| `delete_prefix` | `sheet`, `prefix` (`""` = every key) | `{success, deleted, seq}`; broadcasts `Invalidate{prefix}` |
| `delete_suffix` | `sheet`, `suffix` | `{success, deleted, seq}`; broadcasts `Invalidate{suffix}` |
| `lock` | `sheet`, `key`, `lock_ttl_seconds` | `{success, key}` or `lock held by another client` |
| `unlock` | `sheet`, `key` | `{success, key}` or `not lock owner` / `lock not found` |
| (empty / unknown body) | — | `unknown action` (or `must open sheet first`); the stream stays up |

Every body except `open` is refused `must open sheet first` until a sheet is open. A message over
`REDLITE_MAX_FRAME_BYTES` (default 4 MiB = 4 × the chunk) ends the stream `RESOURCE_EXHAUSTED`; the
reader refuses such a set before sending it (`reader.ErrTooLarge`). A value bigger than a chunk
travels alone in its own state chunk. `dump` has no message: it moved to
`GET /admin/sheets/{id}/export`.

**ServerMsg** `{id, oneof body}`:

| body | fields |
|---|---|
| `reply` | `success, error, sheet, key, value, deleted, count, protocol_version, epoch, seq, expires_in_ms` |
| `state` (`StateChunk`) | `sheet, seq, epoch, entries[{key, value, expires_in_ms}], chunk, last` — sorted by key, at most 1 MiB per chunk (no message near 4 MB at any sheet size); the reader bulk-builds when `last` arrives |
| `replicate` | `sheet, key, value, seq, expires_in_ms` |
| `invalidate` | `sheet, seq`, one of `key`, `keys` (an expiry pass), `prefix`, `suffix` (`""` = whole sheet) |

## Replica.Follow and Replica.Probe

`rpc Follow(stream ReplicaClientMsg) returns (stream ReplicaServerMsg)` and
`rpc Probe(ProbeRequest) returns (ProbeReply)` both require `authorization: Bearer
<REDLITE_CLUSTER_TOKEN>` (D-031); without it: `UNAUTHENTICATED`, nothing sent, no registration.

The replica sends `Register{instance_id, started_at, epoch}`; the leader answers `register_reply
{success, epoch}` (or refuses and steps down to a higher epoch, D-012), then: `snapshot_sheet`
chunks per sheet (`sheet, password_hash, project_id, entries, seq, epoch, last`), `snapshot_end
{sheets, epoch}`, then the live stream — `heartbeat {epoch, instance_id}`, `replicate_sheet`,
`replicate`, `op {kind DELETE|DELETE_PREFIX|DELETE_SUFFIX|EXPIRE, …}`, `control {kind
PROJECT_SUSPENDED|PROJECT_RESUMED|KEY_REVOKED|SESSION_KILL, …}`. `Probe` answers `{role, epoch,
instance_id}`: a leader that sees a peer leader with a higher epoch steps down (the peer fence).

## How a stream ends (D-050)

Every server-ended stream carries a status with a `redlite.v1.ErrorDetail{reason, message,
close_code}` detail. `close_code` keeps the v1 numbers so one vocabulary covers both eras.

| status | reason | close_code | client |
|---|---|---|---|
| PERMISSION_DENIED | SUSPENDED | 4001 | terminal (`reader.ErrSuspended`) |
| UNAUTHENTICATED | KEY_REVOKED | 4002 | terminal (`ErrAdmitted`) |
| ABORTED | KICKED | 4003 | terminal (`ErrKicked`) |
| UNAUTHENTICATED | INVALID_PASSWORD / NOT_AUTHORIZED | 1008 | `ErrAuthFailed` from New; a running reader retries |
| FAILED_PRECONDITION | STALE_EPOCH / NOT_LEADER | 1008 / 1001 | fail over |
| RESOURCE_EXHAUSTED | MESSAGE_TOO_LARGE | 1009 | reconnect |
| RESOURCE_EXHAUSTED | SLOW_CONSUMER | 1001 | reconnect and resync (D-014) |
| UNAVAILABLE | GOING_AWAY | 1001 | reconnect |
| OK | — | — | the client ended the stream |

**Slow consumer.** Each stream has one writer behind a bounded queue (`REDLITE_CONN_SEND_QUEUE`). A
broadcast never waits on a peer's flow control; a full queue ends that stream SLOW_CONSUMER.

**Keepalive.** Server pings an idle connection every `REDLITE_PING_INTERVAL`, drops it after
`REDLITE_WRITE_TIMEOUT` without an ack; clients may ping no more often than every 5 s.

## Health

`grpc.health.v1.Health/Check` on the client port (open, no credentials): SERVING while the node
accepts sessions, NOT_SERVING during shutdown. `GET /health` on the admin port is unchanged: 200
`{status:"ok"}`; `role`, `epoch`, `instance_id` only with the cluster token (D-031); `stats` in
development only (D-040).

## The null placeholder

`set_null` stores the JSON string `"__NULL__"`. A reader's `IsNull(key)` is true for it and `Get`
treats it as a miss the database already answered.

## The retired v1 wire (record)

Until 2026-11-11 the same operations were JSON text frames over WebSocket: `GET /ws` (frames with
`action`, replies with `success`, broadcasts with `type`), `GET /replica` (Bearer header), close
codes 1000/1001/1008/1009/4001/4002/4003, and `/health` + `/admin/*` on the client port. Every v1
action and frame maps to a message above (`dump` and `register_replica`-on-`/ws` excepted, both
already refusals). A v1 reader (advo_cache) cannot connect any more; see
`doc/consumer/backendadvo_migration.md`.
