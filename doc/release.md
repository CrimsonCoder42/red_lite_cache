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

## Next tag: v0.3.1 (leak suite, 2026-10-29)

Patch bump: additive reader API only — `reader.Stats` / `(*Reader).Stats()` (diagnostic, not part of the
compatibility contract); a closed reader now frees its map. No wire change except the development-only
`"stats"` object on `/health` (D-040). New server variable `REDLITE_TENANT_MEMORY_MAX` (default 10 000,
D-041) — add it to `.env.example` (proposal: `doc/bootstrap_2026-10-29/env.example.proposed`).

## Previous: v0.3.0 (consumer conformance, 2026-10-29)

Minor bump: a new public package, `reader/v1compat` (advo_cache's reader surface over `reader.Reader`,
D-035) — backendAdvo's cut-over step 1 is an import-path change to it
(`doc/consumer/backendadvo_migration.md`). `reader/` and `protocol/` unchanged; no wire change.

## Previous: v0.2.0 (tenancy, 2026-10-29)

Minor bump: additive wire (`api_key` on open, close codes 4001–4003, replica control frames) and new
reader API (`Config.APIKey`, `LastError()`, `ErrSuspended` / `ErrAdmitted` / `ErrKicked`). Nothing a
v0.1.0 or v1 reader sends or decodes changed. Server side, two deployment changes:
`REDLITE_CLUSTER_TOKEN` is required in production on both nodes (same value), and production defaults
to `REDLITE_ADMISSION=key` — backendAdvo must set `REDLITE_ADMISSION=password` on the cache until its
reader sends a key. v0.1.0 (bootstrap) is the previous tag.

## Operator runbook — projects, keys, sessions (D-024…D-031)

Every call takes the admin key. `H="Authorization: Bearer $REDLITE_ADMIN_KEY"`, `C=http://leader:8095`.
Writes go to the leader (a replica answers 503 `not leader`); listings and session kills work on
either node. Every mutation is one `audit_events` row when persistence is on.

```bash
# 1. Register a project → its first API key, shown ONCE (hand it to the project over a secret channel)
curl -sH "$H" -d '{"name":"clinic-b","label":"prod-api"}' $C/admin/projects
#   {"project_id":"<P>","name":"clinic-b","api_key":"rlk_…","key_prefix":"rlk_xxxx"}

# 2. Create its sheet (admin key, or the project's own key)
curl -sH "$H" -d '{"name":"clinic-b-prod","password":"<sheet password>"}' $C/admin/projects/<P>/sheets

# 3. The project's reader: reader.Config{Sheet:"clinic-b-prod", Password:…, APIKey:"rlk_…"}

# 4. Look
curl -sH "$H" "$C/admin/projects?limit=50"                 # paged: ?cursor=<next_cursor>
curl -sH "$H" $C/admin/projects/<P>                          # keys (prefix/label/created/revoked/last_used), sheets
curl -sH "$H" "$C/admin/sessions?project=<P>"                # both nodes, each row tagged "node"
curl -sH "$H" $C/admin/sheets/clinic-b-prod/sessions

# 5. Kick one session (close 4003; the reader stops, LastError() = ErrKicked)
curl -sH "$H" -X DELETE $C/admin/sessions/<session_id>

# 6. Suspend / resume (every session closed 4001, open refused, data kept; resume re-admits)
curl -sH "$H" -X POST $C/admin/projects/<P>/suspend
curl -sH "$H" -X POST $C/admin/projects/<P>/resume

# 7. Keys: issue another, rotate one (new key shown once, old refused at once), revoke one (its sessions close 4002)
curl -sH "$H" -d '{"label":"api-instance-3"}' $C/admin/projects/<P>/keys
curl -sH "$H" -X POST $C/admin/keys/<key_id>/rotate
curl -sH "$H" -X DELETE $C/admin/keys/<key_id>

# 8. Revoke the project (keys revoked, sheets and entries dropped — not reversible)
curl -sH "$H" -X POST $C/admin/projects/<P>/revoke
```

**The admin key crosses the wire to the peer** (`GET`/`DELETE /admin/sessions` merge and forward)
over the scheme of `REDLITE_PEER_ADDRESS`: `ws://` becomes plain `http://`. Keep the pair on a private
link (Tailscale) or use `wss://` so the key is not sent in cleartext.

**Rotating the admin key** is an environment change: set the new `REDLITE_ADMIN_KEY` on both nodes and
restart them (replica first, then the leader; readers fail over and back). There is no API for it.
**Rotating the cluster token** is the same, and the two nodes must restart close together: a replica
with the old token is refused by a leader with the new one until both match.

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
air                     # leader on :8095 (live reload)
make dev-pair           # leader :8095 + replica :8096, each fencing the other
docker compose up       # postgres + leader + replica, persistence on
```

Ports are :8095/:8096 so a local advo_cache on :8081/:8082 is never touched. Configuration:
`doc/bootstrap_2026-10-29/env.example.proposed` (until it replaces `.env.example`).
