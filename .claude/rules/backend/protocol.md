---
paths:
  - "protocol/**/*.go"
  - "internal/ws/**/*.go"
  - "reader/**/*.go"
  - "internal/replica/**/*.go"
---

# Wire protocol

_Origin: Anthony GG's TCP command protocol (`doc/reference/distributed_cache.md` § 3–4), advo_cache `ws/handler.go` + `replica/replica.go` (the duplicated types this package exists to merge), code-heim go_12_gorilla_websocket vs go_75_coder_websocket (D-011)._

`protocol/` is the only place a message type is defined. The server (`internal/ws`), the replica
client (`internal/replica`) and the reader (`reader/`) all import it. A struct defined twice is a drift
waiting to happen — `advo_cache` had the replica message types in `replica/replica.go` and the reader
decoding into its own structs, and the two disagreed on `prefix`/`suffix` handling.

## Every message is a typed struct

- `json:"name"` on every field; optional fields `omitempty`. Field names are snake_case on the wire.
- Client → server messages carry `action`. Server → client messages carry `type`. Those two words are
  the discriminators; `doc/protocol/wire.md` has the full table.
- The payload of a `set`/`replicate` is `json.RawMessage`, not `any`: the server stores bytes and never
  decodes tenant data it has no reason to inspect. The reader decodes on `Get` into the caller's type.

## Additive only

A shipped reader (`backendAdvo` pins a tag) keeps working across every server release:

- Add a field: fine, `omitempty`, the old reader ignores it.
- Add an action or type: fine, the old server answers `{"success":false,"error":"unknown action"}` and
  the old reader ignores an unknown type.
- Rename, remove or retype a field: **not fine.** Stop and ask. It needs a `protocol.Version` bump, a
  D-id, and a reader that negotiates.

`doc/protocol/wire.md` lists every field in the v1 wire format that `advo_cache`'s reader and the
current `backendAdvo` read. That list is the compatibility contract.

## The reader is a library

- Exported surface is what a consumer calls: `New`, `Get`, `Set`, `Delete`, `DeletePrefix`,
  `DeleteSuffix`, `Has`, `IsNull`, `Close`, `IsConnected`. Anything else is unexported.
- `reader.Config` is the only input; it takes a `context.Context` for lifetime and an optional
  `*slog.Logger`. No globals, no `init()`, no reading the environment.
- Reconnect forever with capped backoff (`advomedix-v2/backendAdvo/CLAUDE.md` G-01-15). While
  disconnected, `Get` reports a miss and `IsConnected` is false; the consumer decides what that means.
- Deletes issued while disconnected are **queued and replayed** on reconnect, in order, before the
  first read is served. `advo_cache` dropped them, and that is the bug `backendAdvo/cache/store.go`
  had to work around.
