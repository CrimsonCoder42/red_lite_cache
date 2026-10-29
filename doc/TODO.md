# TODO — known gaps, not yet scoped

- K6 chunked full-state sync for large sheets (SCOPE.md).
- LISTEN/NOTIFY cross-instance sync (persistence report Q14) — only if a second leader is ever wanted.
- Row Level Security on the cache database — only if the database is ever shared.
- Metrics endpoint (`/metrics`, Prometheus) — not in advo_cache; decide when the first consumer asks.
