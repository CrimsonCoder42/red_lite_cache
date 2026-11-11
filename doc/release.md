# Release — how consumers get it

The module is `github.com/CrimsonCoder42/red_lite_cache` (D-001). Consumers import the public packages only:

```go
import (
    "github.com/CrimsonCoder42/red_lite_cache/reader"
    "github.com/CrimsonCoder42/red_lite_cache/protocol"
)
```

`go list -deps ./reader` pulls in this module and google.golang.org/grpc + google.golang.org/protobuf
(and grpc's own dependencies). The server's dependencies (pgx, migrate, bcrypt, errgroup) never reach a
consumer. Non-Go clients generate from `proto/redlite/v1/*.proto`.

## Next tag: v0.4.0 (gRPC wire, 2026-11-11)

Minor bump, **breaking on the wire** (D-048): JSON over WebSocket is removed. `reader` and
`reader/v1compat` APIs are unchanged, but `Config.LeaderAddr`/`BackupAddr` are now `host:port`
(`"cache:8095"`); a `ws://` URL is refused by `New`. A v1 reader (advo_cache) cannot connect. Server:
the client port serves gRPC only; `/health` and `/admin/*` move to `REDLITE_ADMIN_PORT` (default
`:8097`, D-049) — publish it; `REDLITE_LEADER_ADDRESS` / `REDLITE_PEER_ADDRESS` become `host:port` of
the other node's client port; new optional `REDLITE_PEER_ADMIN_ADDRESS`. Load balancers can use
`grpc.health.v1` on the client port. Deploy both nodes together (a v0.3 replica cannot follow a
v0.4 leader).

## Previous: v0.3.1 (leak suite, 2026-10-29)

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

Every call takes the admin key. `H="Authorization: Bearer $REDLITE_ADMIN_KEY"`, `C=http://leader:8097` (the admin port, D-049).
On the docker-compose development stack, steps 1–2 are already done by the bootstrap (`make up`, D-047).
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

**The admin key crosses the wire to the peer** (`GET`/`DELETE /admin/sessions` merge and forward) over
`REDLITE_PEER_ADMIN_ADDRESS` (default `http://<peer host>:<this admin port>`). Keep the pair on a
private link (Tailscale) or set an `https://` peer admin address so the key is not sent in cleartext.
The cluster token likewise rides as gRPC call credentials over plaintext h2c unless TLS is terminated
in front.

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
air                     # leader on :8095 gRPC + :8097 admin (live reload)
make dev-pair           # leader :8095/:8097 + replica :8096/:8098, each fencing the other (Replica/Probe)
make up                 # docker: postgres + leader (persistence on) + the dev-project bootstrap (D-047)
make cluster            # … plus the replica (compose profile "cluster")
make down / make wipe   # stop (data kept) / stop and delete the data (then remove .redlite/)
make smoke              # testing/compose_smoke.sh: the whole stack under its own project, then torn down
```

`make up` (or `docker compose up -d`) leaves a ready development project: the one-shot `bootstrap`
service (`docker/bootstrap/bootstrap.sh`) runs steps 1–2 of the runbook below through the admin API and
writes `./.redlite/dev-project.env` (mode 600). It is idempotent (a second run creates nothing),
resumable (secrets are on disk before the call that needs them), and recovers a project whose key file
was lost by issuing it a new key, never a second project. Ports bind to 127.0.0.1 (leader 8095 +
admin 8097, replica 8096 + admin 8098, postgres 5434); the admin key and cluster token defaults are development-only.

Ports are :8095/:8096 (gRPC) and :8097/:8098 (admin) so a local advo_cache on :8081/:8082 is never touched. Configuration:
`doc/bootstrap_2026-10-29/env.example.proposed` (until it replaces `.env.example`).
