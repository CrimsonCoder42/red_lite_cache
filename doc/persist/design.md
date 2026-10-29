# Persistence — design (distilled)

_Source: `advomedix-v2/advo_cache/CACHE_PERSISTENCE_REPORT.md` (Devin-confirmed decisions, 2026-10). This
is the short version the S5 spec is written from. Rules are in `.claude/rules/backend/go-persist.md`._

## Confirmed decisions (carry into DECISIONS.md as D-ids when S5 starts)

1. **Separate PostgreSQL database** for the cache; never shares `backendAdvo`'s.
2. **`cache_entries` is UNLOGGED** — the memory copy is the source of truth; losing the table on a crash is a cold start.
3. **Write-behind**: bounded channel, one writer goroutine, flush at 100 ops or 5 s.
4. **Per-project API keys**, SHA-256 hashed at rest, issued on project creation, for the HTTP management API.
5. **bcrypt for sheet passwords** (unchanged from advo_cache).
6. **Application-level tenancy**: every table has `project_id`, every query filters on it. No RLS.
7. **Go TTL cleaner**, no `pg_cron`.
8. **Single monolithic full-state sync** for now (K6 in SCOPE.md).

## Tables (draft for `db/migration/000001_init.up.sql`)

```sql
CREATE TABLE projects (
  id          uuid PRIMARY KEY,
  name        text NOT NULL UNIQUE,
  created_at  timestamptz NOT NULL DEFAULT now(),
  quota_keys  bigint,                -- NULL = unlimited
  quota_bytes bigint
);
CREATE TABLE api_keys (
  id          uuid PRIMARY KEY,
  project_id  uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  key_hash    bytea NOT NULL UNIQUE,  -- sha256
  key_prefix  text NOT NULL,          -- first 8 chars, for logs and listing
  created_at  timestamptz NOT NULL DEFAULT now(),
  revoked_at  timestamptz
);
CREATE UNLOGGED TABLE cache_entries (
  project_id  uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  sheet       text NOT NULL,
  key         text NOT NULL,
  value       jsonb NOT NULL,
  expires_at  timestamptz NOT NULL,
  updated_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (project_id, sheet, key)
);
CREATE INDEX cache_entries_expires_idx ON cache_entries (expires_at);
```

## Flows

- **Boot:** migrate → hydrate (`SELECT ... WHERE expires_at > now()`, per sheet, into memory) → start writer and cleaner → listen.
- **Set/Delete:** memory first → enqueue op (non-blocking) → writer batches → one transaction.
- **Shutdown:** cancel ctx → writer flushes what it holds (deadline 10 s) → pool closes.

## Env

```
REDLITE_PERSIST_ENABLED=false
REDLITE_PERSIST_DATABASE_URL=
REDLITE_PERSIST_BATCH_SIZE=100
REDLITE_PERSIST_FLUSH_INTERVAL=5s
REDLITE_PERSIST_CLEANUP_INTERVAL=60s
REDLITE_PERSIST_POOL_MAX=5
REDLITE_PERSIST_POOL_MIN=1
REDLITE_ADMIN_KEY=
```

## Open questions for the S5 spec

- Does a sheet map 1:1 to a project, or does a project own many sheets? (Report assumes many.)
- Does a quota breach reject the `set` or evict oldest? (Report: reject with `{success:false,error:"quota exceeded"}`.)
