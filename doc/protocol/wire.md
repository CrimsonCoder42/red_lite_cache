# Wire protocol — v1

_Source of truth once `protocol/` exists. Until then this is transcribed from
`advomedix-v2/advo_cache/README.md` and `ws/handler.go` (2026-10-29) and is the compatibility contract
every change is measured against._

All frames are JSON text over WebSocket at `/ws`. Client → server frames carry `action`; server →
client frames carry `type` or are a direct reply with `success`.

## Client → server (actions)

| action | fields | reply |
|---|---|---|
| `open` | `sheet`, `password` | `{success, sheet}` then a `sheet_state` frame; wrong password closes the socket |
| `get` | `key` | `{success, key, value}` or `{success:false, error:"key not found"}` |
| `set` | `key`, `value`, `jitter?` | `{success, key}`; broadcasts `replicate` |
| `set_null` | `key` | `{success, key}` |
| `delete` | `key` | `{success, key, deleted}`; broadcasts `invalidate{key}` |
| `delete_prefix` | `prefix` | `{success, deleted}`; broadcasts `invalidate{prefix}` |
| `delete_suffix` | `suffix` | `{success, deleted}`; broadcasts `invalidate{suffix}` |
| `dump` | — | `{success, value, count}` |
| `lock` | `key`, `lock_ttl?` | `{success, key}` or `{success:false, error:"lock held by another client", key}` |
| `unlock` | `key` | `{success, key}` or `{success:false, error:"not lock owner"}` |
| `register_replica` | `instance_id`, `started_at` | `{success}` then a `full_state` frame |

Every action except `open` and `register_replica` is rejected until a sheet is open.

## Server → client (broadcasts, sender excluded)

| type | fields |
|---|---|
| `sheet_state` | `data` (object: key → value) |
| `replicate` | `key`, `value` |
| `invalidate` | one of `key`, `prefix`, `suffix` |

## Leader → replica

| type | fields |
|---|---|
| `heartbeat` | — |
| `full_state` | `sheets` (object: sheet → {key → value}) |
| `replicate` | `sheet`, `key`, `value` |
| `replicate_delete` | `sheet`, `key` |
| `replicate_delete_prefix` | `sheet`, `prefix` |
| `replicate_delete_suffix` | `sheet`, `suffix` |
| `replicate_lock` | `sheet`, `key`, `owner` |
| `replicate_unlock` | `sheet`, `key` |

## The null placeholder

`set_null` stores the literal string `"__NULL__"`. A reader's `IsNull(key)` is true for it and
`Get` treats it as a miss that the database already answered.

## v1 compatibility list

Fields a shipped reader (`advo_cache/reader/reader.go`, pinned by `backendAdvo`) decodes. **Never
rename, remove or retype any of these:**

`action, type, success, error, sheet, password, key, value, prefix, suffix, jitter, lock_ttl,
deleted, count, data, sheets, instance_id, started_at, owner`

## Changes since v1

| Version | Change | Additive | D-id |
|---|---|---|---|
| — | none yet | — | — |
