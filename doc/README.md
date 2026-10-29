# red_lite_cache — docs index

| File | What it holds |
|---|---|
| `scope/SCOPE.md` | What we are building, the slices, their status |
| `specs/<feature>.md` | One spec per slice, from `specs/TEMPLATE.md` |
| `DECISIONS.md` | Every load-bearing choice (D-ids) with the alternative that lost |
| `ASSUMPTIONS.md` | Choices nobody made on purpose; the debt ledger |
| `CHANGELOG.md` | What shipped, with file:line and the test line |
| `TODO.md` | Known gaps, not yet scoped |
| `architecture.md` | Processes, goroutines, data flow, package boundaries |
| `protocol/wire.md` | The wire format: every action, type and field, and the v1 compatibility list |
| `persist/design.md` | The PostgreSQL write-behind layer, distilled from `advo_cache/CACHE_PERSISTENCE_REPORT.md` |
| `reference/` | Outside sources distilled and checked: claims table, defects, what we take and what we replace. `reference/README.md` is the index |
| `handoffs/` | Mid-bug notes; folded and marked stale by `/cleanup` |
| `release.md` | How consumers get it: tagging, `go get`, pinning in `backendAdvo` |

Read before you code. Update when you change. A doc that disagrees with the code is a bug.
