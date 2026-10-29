# Where Postgres earns its place in red_lite_cache

_Research, 2026-10-29. Inputs: the two "replace your stack with Postgres" transcripts (`examples/`, Fireship
and the second one), `advo_cache/CACHE_PERSISTENCE_REPORT.md`, `doc/purpose.md`, `doc/assessment_2026-10-29.md`,
and how backendAdvo actually runs Postgres today (`backendAdvo/docker-compose.yml:5` = `pgvector/pgvector:pg15`,
81 tables, two extensions: `uuid-ossp`, `pg_trgm`; JWT + bcrypt done in Go, `backendAdvo/token/`). Every
Postgres claim is checked against the PostgreSQL manual; the transcripts are the prompt, not the authority._

## The one-paragraph answer

The Go process is the cache. Postgres is **the control plane and the memory that survives a restart**:
who the projects are, which keys open which sheets, what the quotas are, who is leader right now, what
happened (audit), and a durable copy of the hot data so a cold start is not an empty cache. It is **not**
on the read path, not on the write path's latency budget, and not the bus between leader and readers.
Every place the transcripts say "use Postgres instead of X", the question here is "is X something a
cache *server* needs?" — and the answer sorts cleanly into three piles below.

## 1. The two transcripts, claim by claim

| Claim in the transcript | True? | Relevance to a cache server |
|---|---|---|
| **UNLOGGED table as a "poor man's Redis"** — no WAL, faster writes | True, with two consequences the video skips: an UNLOGGED table is **truncated on crash recovery** and **not replicated to a streaming standby** ([CREATE TABLE, UNLOGGED](https://www.postgresql.org/docs/current/sql-createtable.html#SQL-CREATETABLE-UNLOGGED)). | **Use it, eyes open.** `cache_entries` is a copy of memory, so losing it on a crash costs a cold start, not data. But it means a Postgres failover also empties the durable copy. Acceptable for v1 (TTL-bounded data); `REDLITE_PERSIST_LOGGED=true` flips it to a logged table for anyone who wants hydration to survive a DB failover. |
| **pg_cron to expire rows** | True, but it is an extension that needs `shared_preload_libraries` and superuser to install; many managed Postgres offerings gate it ([pg_cron](https://github.com/citusdata/pg_cron)). | **No.** A 10-line Go ticker running `DELETE … WHERE expires_at < now() LIMIT 10000` needs nothing from the host. This is already decision 7 in the persistence report. |
| **JSONB + GIN for unstructured data** | True ([JSON types](https://www.postgresql.org/docs/current/datatype-json.html), [GIN](https://www.postgresql.org/docs/current/gin.html)). | **JSONB yes, GIN no.** Values are opaque to the server (the reader decodes them); we never query *inside* a value. `value jsonb` for compactness and validation; no GIN index — it would cost write throughput on the hottest table for a query nobody runs. |
| **`FOR UPDATE SKIP LOCKED` as a job queue** | True, core since 9.5 ([SELECT, locking clause](https://www.postgresql.org/docs/current/sql-select.html#SQL-FOR-UPDATE-SHARE)). | **Not for the hot path** (the persist writer is one goroutine with a channel; there is nothing to contend for). **Yes for admin jobs** (§3): export, bulk revoke, rehash — a `admin_jobs` table drained with SKIP LOCKED means any instance can run them and a crash mid-job is retried, with no queue service. |
| **Full-text search / pg_trgm** | True. backendAdvo uses `pg_trgm` for the food catalog. | **Not now.** The only search in a cache admin is "find a key by prefix", and that is a `LIKE 'root:%'` on a B-tree with `text_pattern_ops`, not FTS. |
| **pgvector / HNSW** | True. | **No.** Nothing here is a vector. (backendAdvo's image ships it; we don't need the extension.) |
| **PostGIS, BRIN + partitioning for time series** | True ([BRIN](https://www.postgresql.org/docs/current/brin.html), [declarative partitioning](https://www.postgresql.org/docs/current/ddl-partitioning.html)). | **BRIN + monthly partitions for the audit log only** (§3). Not for metrics — those are Prometheus-shaped and belong in `/metrics`, not rows. |
| **Materialized views for dashboards** | True; `REFRESH … CONCURRENTLY` needs a unique index ([REFRESH MATERIALIZED VIEW](https://www.postgresql.org/docs/current/sql-refreshmaterializedview.html)). | **Later.** A per-project usage view (keys, bytes, ops/day) refreshed by the cleaner tick is the natural "admin dashboard" and costs one migration when someone asks. |
| **PostgREST / pg_graphql as the API** | True that they exist. | **No.** The admin API is six endpoints behind API-key auth; a generated REST layer over the registry tables would expose `api_keys.key_hash` by default and add a second process to operate. |
| **Roll your own auth in SQL: pgcrypto `crypt()` + pgjwt** | pgcrypto is contrib and real; pgjwt is third-party. Hashing in SQL means the plaintext secret travels to the DB and can land in `log_statement`/`pg_stat_statements`. | **No.** bcrypt (sheet passwords, as advo_cache does) and sha256 (API keys) stay in Go, exactly as backendAdvo does in `token/` and with `golang.org/x/crypto/bcrypt`. Postgres stores hashes, never sees secrets. |
| **Row Level Security to scope tenants** | True ([RLS](https://www.postgresql.org/docs/current/ddl-rowsecurity.html)). Needs one DB role per tenant or `SET LOCAL` a session variable per transaction. | **Not v1** (persistence report decision 6: `WHERE project_id = $1` everywhere, sqlc makes it hard to forget). RLS is the right *defence in depth* once a second project exists and a human can run ad-hoc SQL on the DB — one migration, `TODO.md`. |
| **LISTEN/NOTIFY for realtime** (implied by "sync layer") | True; payload < 8000 bytes, not durable, delivered only to connected sessions ([NOTIFY](https://www.postgresql.org/docs/current/sql-notify.html)). | **No for data, maybe for control.** Fan-out to readers stays on WebSocket (the entire point of the product). NOTIFY is a cheap way to tell *every cache instance* "project X was revoked" or "quota changed" without them polling — the one place a Postgres-side bus fits (§2.4). |
| **"Until you need sub-millisecond in-memory caching for millions of websockets, just use Postgres"** | The second transcript's own caveat. | This project *is* that specialised tool for backendAdvo. The honest version of the video's thesis, applied here: **Postgres for everything except the thing we are building.** |

Not in either video but the most important Postgres feature for this system:
**advisory locks** (`pg_try_advisory_lock`, session-scoped, released on disconnect —
[Advisory Locks](https://www.postgresql.org/docs/current/explicit-locking.html#ADVISORY-LOCKS)). See §2.3.

## 2. The three piles

### 2.1 Postgres owns it (source of truth lives there)

| Concern | Why Postgres | Tables / mechanism |
|---|---|---|
| **Identity: projects, API keys, sheets, who may open what** | This is the "sign-in and authorization" the user named. It must survive restarts, be editable by an admin, be auditable, and be the same on the leader and the replica. Memory cannot do any of that. | `projects`, `api_keys` (sha256 hash + prefix + revoked_at), `sheets` (project_id, name, bcrypt hash, created_by_key). `open` checks the bcrypt in memory against a hash loaded from here; a revoked key or sheet is pushed to instances via NOTIFY and also re-read on the cleaner tick. |
| **Quotas and limits per project** | Policy, not data. Set by an admin, read rarely, enforced in memory. | `projects.quota_keys`, `quota_bytes`, `rate_limit_per_s`. Memory keeps counters; Postgres keeps the ceiling. A breach answers `{"success":false,"error":"quota exceeded"}`. |
| **Cluster state: epoch, who is leader, since when** | D-012 needs the epoch to survive a full restart of both nodes, or a rejoining old leader cannot tell it is stale. | `cluster_state(id=1, epoch bigint, leader_instance text, changed_at)`. Written on promotion, read on boot. |
| **Audit of admin actions and cluster events** | "Who created project X", "when did the replica promote", "who revoked key Y" are compliance questions, and the answer must outlive the process. | `audit_events(at timestamptz, actor text, action text, subject text, detail jsonb)`, partitioned by month, BRIN on `at`. Never read on the hot path. |
| **Durable copy of sheet contents** | A cold start with an empty cache is a stampede on backendAdvo's database. Hydration turns a restart into a warm start. | `cache_entries` (UNLOGGED by default), write-behind per `go-persist.md`. |

### 2.2 Go owns it (Postgres must stay out)

| Concern | Why not Postgres |
|---|---|
| **Every `get`** | The whole product is "O(1) local read, no network". A read that touches Postgres is a design failure, not an optimisation target. |
| **Set / delete / broadcast ordering** | The per-sheet mutex and send queues (D-014) decide order in microseconds. A round trip to Postgres would add milliseconds *and* a second ordering authority. Persist sees ops after the fact. |
| **TTL expiry** | Lazy on read + one janitor (D-003). The cleaner on the Postgres side only keeps the durable table tidy; it never drives invalidation. |
| **Locks (`lock` / `unlock`)** | Sub-millisecond, owned by a connection, released on disconnect — in-memory on the leader. Postgres advisory locks would be the alternative if locks had to survive a leader failover; they don't (advo_cache never did, backendAdvo never calls them). |
| **Rate limiting** | `x/time/rate` buckets per project in memory (`go-rate-limiting.md`); the limit value comes from `projects`. |
| **Secrets** | Hash in Go, store the hash. Postgres never receives a password or key in plaintext. |

### 2.3 Postgres as the referee (optional, decided per deployment)

**Leader election with an advisory lock.** D-012 chose an in-protocol epoch so failover works with
persistence off. When persistence is on, Postgres can *also* be the tie-breaker: on boot and on heartbeat
timeout, an instance runs `SELECT pg_try_advisory_lock(hashtext('redlite-leader'))` on a dedicated
connection; whoever holds it is leader, the session lock drops when that connection dies, and a
partitioned old leader that cannot reach Postgres cannot keep the lock. This is simpler and stronger than
an epoch alone, and the two compose: the lock decides, the epoch fences. Trade-off: it makes Postgres
availability part of leader availability. Recommendation: ship the epoch (D-012) now; add
`REDLITE_ELECTION=advisory` in S4 as the mode a Postgres-backed pair runs with. This is the TODO.md item D-012 named.

### 2.4 Postgres as the control bus (small, deliberate)

`NOTIFY redlite_control, '{"kind":"key_revoked","project":"…"}'` from the admin API after a commit;
every instance `LISTEN`s on one dedicated connection and applies the change to memory. Payload is a
reference, never data. Falls back to the cleaner-tick re-read if the connection was down (NOTIFY is not
durable). This replaces nothing in the WebSocket layer; it only saves polling the registry.

## 3. What this adds to the S5/S6 specs

1. **Schema (first pair, 000001):** `projects`, `api_keys`, `sheets`, `cluster_state`, `cache_entries`
   (UNLOGGED), `audit_events` (partitioned monthly, BRIN). `admin_jobs` deferred to S6 if export needs it.
2. **sqlc packages:** `registry` (projects/api_keys/sheets), `entries`, `cluster`, `audit`.
3. **Config:** `REDLITE_PERSIST_LOGGED` (default false), `REDLITE_ELECTION=epoch|advisory` (default epoch),
   `REDLITE_CONTROL_LISTEN` (default true when persist on).
4. **Decisions to log:** D-019 Postgres role (this doc: control plane + durable copy, never the read/write
   path); D-020 UNLOGGED by default with the failover caveat; D-021 secrets hashed in Go only; D-022
   NOTIFY as control bus, never data.
5. **Tests:** hydration after restart (PERSIST loop) · UNLOGGED table empty after a simulated crash is a
   documented behaviour, not a failure · revoke key → every instance refuses `open` within one tick ·
   advisory election: two instances, one lock, kill the holder, the other acquires.
6. **Postgres version:** backendAdvo runs 15. Nothing above needs newer; target `>= 15`, test on 16 in
   compose (`postgres:16-alpine`, no pgvector image — we don't use it).

## 4. What to resist

- Moving the sheet's hot data into Postgres "because it has UNLOGGED tables" — that is the video's poor
  man's Redis, and the whole reason this project exists is that backendAdvo needs the rich man's version.
- Extensions that need superuser (`pg_cron`, `pgjwt`): they make the deployment target narrower for a
  product meant to be adopted by other projects.
- A generated API over the registry (PostgREST/pg_graphql): one more process, and it leaks hash columns
  unless someone remembers the view.
- RLS before there is a second tenant or a human with psql access: it doubles the test matrix for a
  guarantee `WHERE project_id = $1` already gives via sqlc.

## Sources

- [PostgreSQL: CREATE TABLE — UNLOGGED](https://www.postgresql.org/docs/current/sql-createtable.html#SQL-CREATETABLE-UNLOGGED)
- [PostgreSQL: Advisory Locks](https://www.postgresql.org/docs/current/explicit-locking.html#ADVISORY-LOCKS)
- [PostgreSQL: NOTIFY](https://www.postgresql.org/docs/current/sql-notify.html) · [LISTEN](https://www.postgresql.org/docs/current/sql-listen.html)
- [PostgreSQL: SELECT — The Locking Clause (SKIP LOCKED)](https://www.postgresql.org/docs/current/sql-select.html#SQL-FOR-UPDATE-SHARE)
- [PostgreSQL: Row Security Policies](https://www.postgresql.org/docs/current/ddl-rowsecurity.html)
- [PostgreSQL: BRIN Indexes](https://www.postgresql.org/docs/current/brin.html) · [Table Partitioning](https://www.postgresql.org/docs/current/ddl-partitioning.html)
- [PostgreSQL: JSON Types](https://www.postgresql.org/docs/current/datatype-json.html) · [GIN Indexes](https://www.postgresql.org/docs/current/gin.html)
- [PostgreSQL: REFRESH MATERIALIZED VIEW](https://www.postgresql.org/docs/current/sql-refreshmaterializedview.html)
- [pg_cron](https://github.com/citusdata/pg_cron) · [pgcrypto](https://www.postgresql.org/docs/current/pgcrypto.html)
- `advo_cache/CACHE_PERSISTENCE_REPORT.md` Parts 2–3 · `backendAdvo/docker-compose.yml:5`, `backendAdvo/token/`
