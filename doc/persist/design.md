# Persistence — design (distilled)

## What shipped (bootstrap 2026-10-29)

_Code wins where it differs from the draft below; differences are listed._ Unverified against a real
Postgres on this machine: every DB test SKIPPED (no `REDLITE_TEST_DATABASE_URL`).

- **Schema** `db/migration/000001_init.up.sql`: `projects` (+ seeded `default` project for autocreated
  sheets), `api_keys`, `sheets` (bcrypt hash, project_id; name globally unique), `cache_entries`
  (UNLOGGED, `project_id` + `sheet` + `key`, `expires_at` **nullable** = no expiry, FK to `sheets`),
  `cluster_state` (epoch). `audit_events` is the proposed `000002` pair (`doc/bootstrap_2026-10-29/000002_audit.proposed.sql`, D-019).
- **Writer** `internal/persist/writer.go`: one goroutine, bounded channel (`REDLITE_PERSIST_QUEUE_SIZE`),
  non-blocking enqueue (full → drop + rate-limited Warn), flush on batch size or interval in one
  transaction, collapsed per key (`plan.go`), retried once then dropped with an Error. Sheet creates are
  never dropped (parked and flushed first) so the FK cannot poison a batch. Only the leader writes.
- **Prefix/suffix deletes** use `starts_with()` / `right()` — not `LIKE`, which `_`/`%` in a key would widen.
- **Hydration** before the listener (`internal/app/wire_replica.go`, leader only): every sheet with its
  hash, then live rows per sheet; expired rows are not loaded.
- **Cleaner** one goroutine, `DELETE … LIMIT 10000` per `REDLITE_PERSIST_CLEANUP_INTERVAL`.
- **Epoch** `cluster_state` via `RaiseEpoch` (never goes backwards) on boot and promotion.
- **Migrations** embedded (`db/store.go`, iofs) and applied at boot by golang-migrate's pgx v5 driver.
- Decisions: D-013, D-015, D-019…D-022; open questions below are answered: a project owns many sheets;
  quota columns exist but are not enforced yet (`doc/TODO.md`).

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
