# red_lite_cache

A multi-tenant cache with a gRPC wire (`proto/redlite/v1`, D-048) and an embeddable Go reader, leader/replica replication with fenced
promotion, distributed locks, and PostgreSQL write-behind persistence. The successor to `advo_cache`.

**Status:** v0.1.0 candidate (bootstrap 2026-10-29): S1–S6 built, `bash testing/run.sh` green with
`-race`. `doc/scope/SCOPE.md` is the plan; `doc/purpose.md` the requirements.

## Quick start — one command to a ready cache

```bash
make up              # = mkdir -p .redlite && docker compose up -d --build && docker compose wait bootstrap
```

That starts PostgreSQL (schema migrated by the server at boot), the leader with persistence on, and a
one-shot bootstrap that registers a development project and its first sheet through the admin API.
The connection details land in **`./.redlite/dev-project.env`** (mode 600, gitignored; the key never
appears in the logs):

```
REDLITE_DEV_PROJECT_ID=…   REDLITE_DEV_API_KEY=rlk_…   REDLITE_DEV_SHEET=dev
REDLITE_DEV_SHEET_PASSWORD=…   REDLITE_DEV_ADDR=127.0.0.1:8095
```

```go
r, err := reader.New(reader.Config{
    LeaderAddr: os.Getenv("REDLITE_DEV_ADDR"),
    Sheet:      os.Getenv("REDLITE_DEV_SHEET"),
    Password:   os.Getenv("REDLITE_DEV_SHEET_PASSWORD"),
    APIKey:     os.Getenv("REDLITE_DEV_API_KEY"),
})
```

| | |
|---|---|
| Stop, keep the data | `docker compose down` (`make down`) |
| Stop and wipe the data | `docker compose down -v` (`make wipe`), then remove `.redlite/` — its key is dead |
| Add the replica (127.0.0.1:8096, admin 8098) | `docker compose --profile cluster up -d` (`make cluster`) |
| Prove the whole stack (own project, own env file, ports 18095–18098/15433, torn down after) | `bash testing/compose_smoke.sh` (`make smoke` also loads `.env` into make) |
| The test suite with the persistence tests on | `make test-persist` (uses `redlite_test` on 127.0.0.1:5434) |

Plain `docker compose up -d` works too: the stack runs with no `.env`. Every setting has a default, and
a `.env` overrides it. **Dev only:** the admin key (`dev-admin-key`), the cluster token and the
postgres password are public development defaults; ports bind to 127.0.0.1 only. Set
`REDLITE_ADMIN_KEY`, `REDLITE_CLUSTER_TOKEN` and `REDLITE_PG_PASSWORD` in `.env` before exposing
anything. On Linux use `make up` (it runs the bootstrap as your user so it can write `.redlite/`).

Without Docker:

```bash
air                  # one leader on :8095 (gRPC) + :8097 (admin) with live reload (reads .env if present)
make dev-pair        # leader :8095/:8097 + replica :8096/:8098 under air
bash testing/run.sh  # every package under -race, logged to testing/runs/
```

Never :8081/:8082 (left for a local advo_cache); Postgres is on host port 5434 (a native PostgreSQL
often holds 5433).

## Embedding the reader

```go
import "github.com/CrimsonCoder42/red_lite_cache/reader"

r, err := reader.New(reader.Config{
    LeaderAddr: "localhost:8095", // gRPC (Cache.Session)
    BackupAddr: "localhost:8096", // optional: the replica, used after a promotion
    Sheet:      "patient-access",
    Password:   os.Getenv("CACHE_PASSWORD"),
})
if err != nil { // only a refused password; a cache that is down at boot reconnects in the background
    return err
}
defer r.Close()

var p Patient
if r.Get("patient:"+id, &p) { // O(1), local, decoded once into your type
    return p, nil
}
p = loadFromDB(id)
_ = r.SetWithTTL(ctx, "patient:"+id, p, 15) // minutes; ErrStale = a sweep raced this read-through, don't cache
...
_ = r.DeletePrefix(ctx, "patient:"+id) // never dropped: queued and replayed if the link is down
```

Ports: **8095/8096** gRPC (`redlite.v1.Cache`, `redlite.v1.Replica`, `grpc.health.v1`), **8097/8098**
admin HTTP (`GET /health`, `/admin/*`). The contract is `proto/redlite/v1/*.proto` (`make proto`);
`doc/protocol/wire.md` describes it. Any language with a gRPC generator can be a client
(`testing/blackbox` drives the server with nothing but an address and the `.proto`).

## A second project, no code change

```bash
curl -H "Authorization: Bearer $REDLITE_ADMIN_KEY" -d '{"name":"clinic-b"}' localhost:8097/admin/projects
# → {"project_id":"…","api_key":"rlk_…"}   (shown once)
curl -H "Authorization: Bearer rlk_…" -d '{"name":"clinic-b-prod","password":"…"}' \
     localhost:8097/admin/projects/<project_id>/sheets
curl -H "Authorization: Bearer rlk_…" localhost:8097/admin/sheets/clinic-b-prod/export
```

## Docs

- Index: `doc/README.md` · Architecture: `doc/architecture.md` · Wire protocol: `doc/protocol/wire.md`
- Decisions: `doc/DECISIONS.md` · Releasing and pinning: `doc/release.md`
