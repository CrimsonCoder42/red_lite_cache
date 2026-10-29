# Wire protocol — v1 (with additive changes)

_Code: `protocol/messages.go` is the source of truth; this file describes it. v1 columns are transcribed
from `advomedix-v2/advo_cache/README.md` and `ws/handler.go` (2026-10-29) and are the compatibility
contract every change is measured against. `protocol.Version` is still 1: every change below is additive._

All frames are JSON text over WebSocket. Clients speak at `/ws`; a replica speaks to its leader at
`/replica` (D-010). Client → server frames carry `action`; server → client broadcasts carry `type`; a
direct reply to a request carries neither and has `success`. A client tells a reply from a broadcast
by the presence of `type`, and matches a reply to its request by `id` when it sent one.

Every request may carry `id` (echoed in the reply) and `epoch` (the highest leader epoch the sender has
seen; a server with a lower epoch refuses with `stale epoch`, D-012).

## Client → server (actions, `/ws`)

| action | v1 fields | new fields | reply |
|---|---|---|---|
| `open` | `sheet`, `password` | `version` (absent = 1) | `{success, sheet, protocol_version, epoch, seq}` then a `sheet_state` frame. Wrong password → `{success:false,error:"invalid password"}` and the socket closes. Unknown sheet with `REDLITE_SHEET_AUTOCREATE=false` → `sheet not found`. A replica that has not promoted → `not leader` |
| `get` | `key` | — | `{success, key, value}` or `{success:false, error:"key not found"}` |
| `set` | `key`, `value`, `ttl?` (MINUTES, int; 0/absent = `REDLITE_CACHE_TTL`), `jitter?` (accepted, ignored: jitter is server policy) | `base_seq?` (R6) | `{success, key, seq, expires_in_ms}`; broadcasts `replicate`. Refused: `stale write` (base_seq older than a covering write) or `sheet full` |
| `set_null` | `key` | — | `{success, key, seq, expires_in_ms}`; stores `"__NULL__"`, broadcasts `replicate` |
| `delete` | `key` | — | `{success, key, deleted, seq}`; broadcasts `invalidate{key}` |
| `delete_prefix` | `prefix` (`""` = every key) | — | `{success, deleted, seq}`; broadcasts `invalidate{prefix}` |
| `delete_suffix` | `suffix` | — | `{success, deleted, seq}`; broadcasts `invalidate{suffix}` |
| `dump` | — | — | `{success:false, error:"dump moved to GET /admin/sheets/{id}/export"}` — the O(n) dump left the client socket |
| `lock` | `key`, `lock_ttl?` (SECONDS) | — | `{success, key}` or `{success:false, error:"lock held by another client", key}` |
| `unlock` | `key` | — | `{success, key}` or `{success:false, error:"not lock owner"}` / `"lock not found"` |
| `register_replica` | `instance_id`, `started_at` | — | on `/ws`: `{success:false, error:"register_replica moved to /replica"}` |

Every action except `open` is rejected with `must open sheet first` until a sheet is open. Malformed
JSON → `invalid JSON`; an unknown action → `unknown action`; a frame over `REDLITE_MAX_FRAME_BYTES`
closes the socket (status 1009).

## Server → client (broadcasts, sender excluded)

| type | v1 fields | new fields |
|---|---|---|
| `sheet_state` | `data` (object: key → value) | `expires_in_ms` (key → remaining ms; absent key = no expiry), `seq`, `epoch` |
| `replicate` | `key`, `value` | `seq`, `expires_in_ms` |
| `invalidate` | one of `key`, `prefix`, `suffix` | `keys` (array: keys expired in one janitor pass, D-003), `seq`. `prefix`/`suffix` are emitted even when `""` |

`seq` is the sheet's sequence number after the write; it increases by one per applied write or
expiry pass, in the order every collaborator receives them (D-014).

## Leader → replica (`/replica`)

The replica sends `register_replica{instance_id, started_at, epoch}`. The leader replies
`{success, epoch}` (or refuses and steps down when the replica's epoch is higher, D-012), then:

| type | fields |
|---|---|
| `snapshot_sheet` | `sheet`, `password_hash` (bcrypt, base64), `project_id`, `data`, `expires_in_ms`, `seq`, `epoch` — one frame per sheet (K6) |
| `snapshot_end` | `sheets` (count), `epoch` — everything after it is the live stream |
| `heartbeat` | `epoch`, `instance_id` |
| `replicate_sheet` | `sheet`, `password_hash`, `project_id`, `seq`, `epoch` — a sheet was created |
| `replicate` | `sheet`, `key`, `value`, `seq`, `epoch`, `expires_in_ms` |
| `replicate_delete` | `sheet`, `key`, `seq`, `epoch` |
| `replicate_delete_prefix` | `sheet`, `prefix`, `seq`, `epoch` |
| `replicate_delete_suffix` | `sheet`, `suffix`, `seq`, `epoch` |
| `replicate_expire` | `sheet`, `keys`, `seq`, `epoch` |

No longer sent: `full_state` (replaced by the chunked snapshot), `replicate_lock` / `replicate_unlock`
(locks belong to connections, which a failover drops). See `doc/ASSUMPTIONS.md`.

## HTTP

| method + path | body |
|---|---|
| `GET /health` | `{status:"ok", role, epoch, instance_id}` — a peer reads `role`/`epoch` to fence a stale leader |
| `/admin/*` | the management API (S6), `doc/specs/s6_api.md` |

## The null placeholder

`set_null` stores the literal JSON string `"__NULL__"`. A reader's `IsNull(key)` is true for it and
`Get` treats it as a miss that the database already answered.

## v1 compatibility list

Fields a shipped reader (`advo_cache/reader/reader.go`, pinned by `backendAdvo`) decodes. **Never
rename, remove or retype any of these** (`protocol/messages_test.go` asserts the names):

`action, type, success, error, sheet, password, key, value, ttl, prefix, suffix, jitter, lock_ttl,
deleted, count, data, sheets, instance_id, started_at, owner`

(`sheets` and `owner` belonged to `full_state` / `replicate_lock`, which only a v1 replica decoded.)

## Changes since v1

| Version | Change | Additive | D-id |
|---|---|---|---|
| 1 | `id` on every request and reply | yes (omitempty) | ASSUMPTIONS: request id |
| 1 | `version` on `open`; `protocol_version` on the open reply | yes | D-010 |
| 1 | `epoch` on requests, open/register replies, `sheet_state` and every replica frame; `stale epoch`, `not leader` errors | yes | D-012 |
| 1 | `seq` on replies, `sheet_state`, `replicate`, `invalidate` and replica frames | yes | D-014 |
| 1 | `expires_in_ms` on set replies, `sheet_state`, `replicate` and replica frames | yes | D-008 |
| 1 | `keys` on `invalidate` (expiry batch) | yes | D-003 |
| 1 | `base_seq` on `set`; `stale write` error | yes | ASSUMPTIONS: R6 mechanism |
| 1 | `sheet full`, `sheet not found` errors | yes | D-013 |
| 1 | `dump` answers an error naming `GET /admin/sheets/{id}/export` | behaviour change, no field change | bootstrap request |
| 1 | replicas on `/replica`: `snapshot_sheet`, `snapshot_end`, `replicate_sheet`, `replicate_expire`; `full_state`, `replicate_lock`, `replicate_unlock` no longer sent | v1 *clients* unaffected; a v1 replica cannot attach | D-010, ASSUMPTIONS: replica stream |
| 1 | `invalidate` emits `prefix`/`suffix` even when `""` (whole-sheet flush) | a v1 reader reads `""` as nothing to do | D-005 |
