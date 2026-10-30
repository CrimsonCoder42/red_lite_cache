# red_lite_cache — docs index

| File | What it holds |
|---|---|
| `purpose.md` | Why this exists: the one consumer, the API it really calls, requirements R1–R12 from its workarounds |
| `audit_2026-10-29.md` | Post-bootstrap audit: every K/R row verified in code; what remains (replica auth, R6 simplification, eviction policy, R7 index, Postgres unproven) |
| `assessment_2026-10-29.md` | advo_cache vs the examples vs production patterns: where it succeeds, where it falls short, what hurts most; K9–K16 |
| `scope/SCOPE.md` | What we are building, the slices, their status |
| `specs/<feature>.md` | One spec per slice, from `specs/TEMPLATE.md` |
| `DECISIONS.md` | Every load-bearing choice (D-ids) with the alternative that lost |
| `ASSUMPTIONS.md` | Choices nobody made on purpose; the debt ledger |
| `CHANGELOG.md` | What shipped, with file:line and the test line |
| `TODO.md` | Known gaps, not yet scoped |
| `architecture.md` | Processes, goroutines, data flow, package boundaries |
| `protocol/wire.md` | The wire format: every action, type and field, and the v1 compatibility list |
| `persist/postgres_role.md` | Where Postgres earns its place: control plane + durable copy; the two transcripts' claims checked; what to resist |
| `persist/design.md` | The PostgreSQL write-behind layer, distilled from `advo_cache/CACHE_PERSISTENCE_REPORT.md` |
| `reference/` | Outside sources distilled and checked: claims table, defects, what we take and what we replace. `reference/README.md` is the index |
| `handoffs/` | Mid-bug notes; folded and marked stale by `/cleanup` |
| `bootstrap_2026-10-29/` | The bootstrap work folder: the agents' brief and contracts, S1 benchmark output, the proposed `.env.example` and `000002_audit` migration waiting for Devin |
| `release.md` | How consumers get it: tagging, `go get`, pinning in `backendAdvo` |

Read before you code. Update when you change. A doc that disagrees with the code is a bug.
