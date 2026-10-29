# doc/reference — where the ideas came from, checked

Every file here distils one outside source into: the pattern, the code as it was actually written, each
claim checked against an authoritative reference, the defects, and the two lists that matter —
**what red_lite_cache takes from it** and **what it replaces, and why**.

| File | Source | Covers |
|---|---|---|
| `distributed_cache.md` | Anthony GG livestream (`examples/distributed_cache_in_golang.md`) | TCP cache, custom protocol, leader → follower write forwarding |
| `codeheim_patterns.md` | 46 code-heim repos | sync.Map cache, memcache read-through, redis lock, gorilla + coder websocket, confinement, worker pool, pipeline, errgroup, pgx, rate limiting, slog, config, testing |
| `lru_cache.md` | "Build Your Own Cache" (`examples/build_your_own_cache.md`) | LRU with `container/list` — **not started** |
| `advo_cache.md` | `advomedix-v2/advo_cache` (read-only) | the direct predecessor: what each package does, file:line, K1–K8 — **not started**; SCOPE.md carries the summary until then |

## How to use these

1. **Before building a slice**, read the reference section for that feature. `/work` Phase 0 names it.
2. **In code**, cite the origin and the change in one comment:
   `// after code-heim go_18_worker_pool: one worker, ctx-cancelled, flush on size or interval (go-persist.md)`.
3. **In a spec**, the Reuse section names the reference section the shape comes from.
4. **These sources are older than the toolchain.** Go 1.18–1.23 snippets, 2023–2025. Every one is a
   starting idea to be checked against current Go (1.27) and the current library release, never copied.
   The "checked" tables carry the verdicts; when a verdict says *dated*, the rule file says what is current.
5. **When a reference and a rule disagree, the rule wins** (`.claude/rules/backend/*.md`). If the rule is
   wrong, change the rule with a D-id; do not quietly follow the reference.

## Adding one

Same seven sections as `distributed_cache.md`: Source · Architecture · Code reconstructed · Claims
checked · Defects · Takes / replaces · Sources. Mark inferred code `// inferred`. No banter, no
"in this video". Add the row above and the `examples/README.md` row if there is a raw file.
