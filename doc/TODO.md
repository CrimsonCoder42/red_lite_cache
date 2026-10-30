# TODO — known gaps, not yet scoped

- K6 chunked full-state sync for large sheets (SCOPE.md).
- LISTEN/NOTIFY cross-instance sync (persistence report Q14) — only if a second leader is ever wanted.
- Row Level Security on the cache database — only if the database is ever shared.
- Metrics endpoint (`/metrics`, Prometheus) — not in advo_cache; decide when the first consumer asks.
- D-012 alternative: a Postgres advisory lock (`pg_try_advisory_lock`) deciding the leader, for when the epoch-only fence is not enough (bootstrap 2026-10-29).
- Shard the server store if contention matters: `BenchmarkStoreGetParallel` is 155 ns/op at 10 CPUs vs 93 ns at 1 (`doc/bootstrap_2026-10-29/bench_s1.txt`); readers serve Get from their local map, so this is not the consumer's hot path (D-002 keeps one RWMutex).
- Rate-limit `/admin/*` per key (`.claude/rules/backend/go-rate-limiting.md`) and add key revocation/rotation endpoints (S6 left out).
- Persist: a dropped OpCreate (full queue) makes later entry upserts for that sheet fail the FK and poison a batch — fix in the writer (upsert the sheet row first or one tx per sheet).
- Replica path auth: a shared `REDLITE_REPLICA_KEY` checked on `/replica` register (today unauthenticated; the snapshot carries every sheet and its password hash).
- Deeper prefix index: the cache indexes keys by first segment only; consumer roots like `pef:` hold every patient, so `DeletePrefix("pef:…:<uuid>:")` still scans all `pef` keys (bootstrap review finding 6).
- R6 under write load: the sweep log holds 4096 entries per sheet; above ~136 writes/s on one sheet the floor passes a 30 s read-through's base and sets are refused (never stale) — raise `REDLITE_SWEEP_LOG_MAX` or bound the stale scan to entries newer than the base (review finding 5).
- Sheet names are global: project B learns a name exists from a 409 (review finding 10). Namespacing names per project is an R11 follow-up.
- Audit log (D-019): proposed `000002_audit` pair in `doc/bootstrap_2026-10-29/000002_audit.proposed.sql`, waiting on the migration checkpoint.
- Tenancy: replicate projects and keys over the stream when persistence is off (today a promoted replica in key mode admits nobody); reconcile key revokes on the cleaner tick (a missed NOTIFY + stream frame leaves the other node's sessions on that key open).
- Tenancy: a reader whose key was revoked while it was disconnected retries `not authorized` forever (no 4002 reaches it) — consider making a refused open after a successful one terminal.
- Tenancy: run the key lookup for unknown sheets too (or a constant-time delay) so open latency does not reveal which sheet names exist; carry the suspended set in the replica snapshot when persistence is off.
