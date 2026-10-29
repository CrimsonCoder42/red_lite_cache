---
paths:
  - "internal/persist/**/*.go"
  - "db/migration/**"
---

# Persistence — write-behind to PostgreSQL

_Origin: advo_cache `CACHE_PERSISTENCE_REPORT.md` (design), code-heim go_74_pgx (pool + tx shape) and go_18_worker_pool (the writer) — `doc/reference/codeheim_patterns.md` § Tier 2–3._

The in-memory sheet is the source of truth for reads. PostgreSQL is the durable copy that survives a
restart and the audit trail a project owner can export. Every rule follows from that order.

## The memory copy wins

- A read never touches the database. `Get` is the hot path; it reads the sheet's map and nothing else.
- Hydration runs once, at boot, before the WS listener accepts a connection. A row whose `expires_at` is
  in the past is skipped, not loaded-then-expired.
- A write goes to memory first and returns. The persist writer sees it afterwards.

## The writer is one goroutine with a bounded channel

- `Set`/`Delete`/`DeletePrefix` enqueue an op on a buffered channel and return. **Enqueue is non-blocking**:
  a full channel drops the op and logs `Persist: enqueue dropped` at Warn with `queue_len` and `cap`.
  The memory copy is still correct; only durability lagged. Never block the WS goroutine on the database.
- One writer goroutine drains the channel into a batch and flushes on `REDLITE_PERSIST_BATCH_SIZE` or
  `REDLITE_PERSIST_FLUSH_INTERVAL`, whichever first. It selects on `ctx.Done()` and flushes what it
  holds before exiting; shutdown waits for that flush with a deadline.
- A batch is one transaction. A failed flush is logged at Error and the batch is **retried once** on the
  next tick, then dropped with a count. Never spin.
- Deletes and sets for the same key in one batch collapse to the last op. A prefix delete is an SQL
  `DELETE ... WHERE key LIKE $1 || '%'`, never a loaded-then-deleted loop.

## Schema

- `cache_entries` is `UNLOGGED`: it is a copy, so losing it on a crash costs a cold start, not data.
  `projects` and `api_keys` are logged tables.
- Every table carries `project_id`; every query filters on it. Application-level tenancy, no RLS (D-id in
  `doc/DECISIONS.md`).
- Migrations are `golang-migrate` files under `db/migration/`, `NNNNNN_name.up.sql` + `.down.sql`,
  applied by `cmd/redlite` at boot when `REDLITE_PERSIST_ENABLED=true`. Agents never edit a migration that
  has shipped; a schema change is a new pair. The hook and the delete guard both protect that folder.

## TTL cleaner

One goroutine, one `DELETE FROM cache_entries WHERE expires_at < now()` every
`REDLITE_PERSIST_CLEANUP_INTERVAL`, limited to `LIMIT 10000` per pass so a backlog never holds a long
lock. It does not touch memory; memory expiry is lazy-on-read plus the cache janitor.

## Pool

`pgxpool` with `REDLITE_PERSIST_POOL_MAX` (default 5) and `REDLITE_PERSIST_POOL_MIN` (default 1). One
instance, one pool, created in `cmd/redlite` and passed in. Nothing opens its own connection.

## Tests

Persist tests need `REDLITE_TEST_DATABASE_URL`. They create a schema named for the test run, migrate
into it, and drop it in cleanup. Without the variable they `t.Skip` with the exact string the harness
greps for: `SKIPPED: persist (no REDLITE_TEST_DATABASE_URL)`.
