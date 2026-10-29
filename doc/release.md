# Release — how consumers get it

The module is `github.com/CrimsonCoder42/red_lite_cache` (D-001). Consumers import the public packages only:

```go
import (
    "github.com/CrimsonCoder42/red_lite_cache/reader"
    "github.com/CrimsonCoder42/red_lite_cache/protocol"
)
```

`go list -deps ./reader` pulls in exactly two modules: this one and `github.com/coder/websocket`. The
server's dependencies (pgx, migrate, bcrypt, errgroup) never reach a consumer.

## Next tag: v0.1.0 (bootstrap, 2026-10-29)

Minor bump from v0.0.1: the first tag with behaviour. Wire changes since v1 are all additive
(`doc/protocol/wire.md` "Changes since v1"); a v1 advo_cache reader still works against this server.

## Tagging (Devin runs these; agents never run git writes)

```bash
git tag v0.1.0
git push origin main --tags
```

Semver: a patch is a fix, a minor adds a field/action (additive), a major is a wire break (D-id required).

## Pinning in backendAdvo (S7)

```bash
cd advomedix-v2/backendAdvo
go get github.com/CrimsonCoder42/red_lite_cache/reader@v0.1.0
go mod tidy
```

Then drop the `replace … => ../advo_cache` line and the `COPY advo_cache/` step in its Dockerfile.

### The reader API change S7 makes (D-016)

Eight of the nine methods backendAdvo calls keep their signatures: `New(Config) (*Reader, error)`,
`Has`, `Set`, `SetWithTTL(ctx, key, value, ttlMinutes int)`, `Delete`, `DeletePrefix`, `DeleteSuffix`,
`Close`, `IsConnected`, and `ErrNotConnected / ErrClosed / ErrAuthFailed`. **`Get` changes:**

| v1 (advo_cache) | v0.1.0 |
|---|---|
| `Get(key) (any, bool)` | `Get(key string, target any) bool` — decodes once into `target`, an isolated copy (R8) |
| — | `GetRaw(key) (any, bool)` — the v1 shape, for any caller that wants it |

`cache.Store.Get` in backendAdvo becomes `return s.reader.Get(key, target)` and loses its
marshal/unmarshal pass.

Behaviour changes a consumer will notice, all intended:

- `Delete*` return nil while disconnected (queued, replayed on reconnect) instead of `ErrNotConnected` (R2).
- `SetWithTTL` waits for the server's ack (R9) and can return `reader.ErrStale` for a read-through
  that raced a sweep (R6). Treat it as "not cached", not as a failure.
- `New` returns a reader that is still reconnecting when the cache is down at boot (R10); only a
  refused password returns an error (`ErrAuthFailed`).

What backendAdvo can then delete (doc/purpose.md R-table status): the DROPPED SWEEPS queue, the
`linkReady` gate, the second sweep 2 s later, the double JSON pass in `Store.Get`, and the
`Logger.Fatal` on cache connect.

## Local development against an unreleased version

In `backendAdvo/go.mod`, temporarily:

```
replace github.com/CrimsonCoder42/red_lite_cache => /Users/devinanderson/Desktop/red_lite_cache
```

Remove the `replace` before committing.

## Running the server

```bash
air                     # leader on :8091 (live reload)
make dev-pair           # leader :8091 + replica :8092, each fencing the other
docker compose up       # postgres + leader + replica, persistence on
```

Ports are :8091/:8092 so a local advo_cache on :8081/:8082 is never touched. Configuration:
`doc/bootstrap_2026-10-29/env.example.proposed` (until it replaces `.env.example`).
