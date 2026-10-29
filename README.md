# red_lite_cache

A multi-tenant WebSocket cache with an embeddable Go reader, leader/replica replication with fenced
promotion, distributed locks, and PostgreSQL write-behind persistence. The successor to `advo_cache`.

**Status:** v0.1.0 candidate (bootstrap 2026-10-29): S1–S6 built, `bash testing/run.sh` green with
`-race`. `doc/scope/SCOPE.md` is the plan; `doc/purpose.md` the requirements.

## Quick start

```bash
air                  # one leader on :8095 with live reload (reads .env if present)
make dev-pair        # leader :8095 + replica :8096 under air
docker compose up    # postgres + leader + replica, persistence on, admin key "dev-admin-key"
bash testing/run.sh  # every package under -race, logged to testing/runs/
curl localhost:8095/health   # {"status":"ok","role":"leader","epoch":1,"instance_id":"..."}
```

Never :8081/:8082 — those are left for a local advo_cache.

## Embedding the reader

```go
import "github.com/CrimsonCoder42/red_lite_cache/reader"

r, err := reader.New(reader.Config{
    LeaderAddr: "ws://localhost:8095/ws",
    BackupAddr: "ws://localhost:8096/ws", // optional: the replica, used after a promotion
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

## A second project, no code change

```bash
curl -H "Authorization: Bearer $REDLITE_ADMIN_KEY" -d '{"name":"clinic-b"}' localhost:8095/admin/projects
# → {"project_id":"…","api_key":"rlk_…"}   (shown once)
curl -H "Authorization: Bearer rlk_…" -d '{"name":"clinic-b-prod","password":"…"}' \
     localhost:8095/admin/projects/<project_id>/sheets
curl -H "Authorization: Bearer rlk_…" localhost:8095/admin/sheets/clinic-b-prod/export
```

## Docs

- Index: `doc/README.md` · Architecture: `doc/architecture.md` · Wire protocol: `doc/protocol/wire.md`
- Decisions: `doc/DECISIONS.md` · Releasing and pinning: `doc/release.md`
