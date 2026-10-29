# red_lite_cache

Single Go module. Repo root: `/Users/devinanderson/Desktop/red_lite_cache`. Module: `github.com/CrimsonCoder42/red_lite_cache`. Go 1.27 (the same toolchain as `advomedix-v2/backendAdvo`).

**What this is.** The successor to `advomedix-v2/advo_cache`: a multi-tenant WebSocket cache
with an embeddable Go reader, leader/replica replication, distributed locks, and (new) PostgreSQL
write-behind persistence. Consumers `go get` it from GitHub; `backendAdvo` is the first.

**Reference sources — three layers, in priority order.**

1. **`advo_cache`** — `/Users/devinanderson/Desktop/advo_production/advomedix-v2/advo_cache`, **read-only**.
   The direct predecessor. Read it for the shape of every feature and its wire protocol, cite the file:line,
   then rebuild it here under the rules below. Never edit it. `CACHE_PERSISTENCE_REPORT.md` there is the
   research behind the persistence layer; `doc/persist/` carries the decisions that came out of it.
2. **`doc/reference/`** — the outside ideas, distilled and checked: `distributed_cache.md` (Anthony GG's
   TCP leader/follower cache), `codeheim_patterns.md` (46 code-heim repos: sync.Map cache, memcache
   read-through, redis lock, gorilla/coder websocket, confinement, worker pool, pipeline, errgroup, pgx,
   rate limiting, slog, config, testing), `lru_cache.md`. Each has a "claims checked" table and a
   "takes / replaces" section. **These are 2023–2025 snippets on Go 1.18–1.23. They are the starting
   idea, never the answer**: check every shape against Go 1.27 and the current library release before
   using it, and when a reference disagrees with a `.claude/rules/` file, the rule wins.
3. **`examples/`** — the raw transcripts the references were distilled from. Inputs only; never cite them.

**Cite the origin in the code.** Every type or function whose shape came from a reference carries one
comment naming it and the change: `// after code-heim go_18_worker_pool: one worker, ctx-cancelled,
flush on size or interval`. A spec's Reuse section names the reference section. That is how a reader six
months from now knows what the initial idea was and why it was changed.

Every request from Devin starts with /work <request>. If a request arrives without /work, run it as if it had.

You operate inside a continuous improvement loop. You do not solve problems in a single pass. You do not skip steps. You do not free-run.

## Layout

| Path | Role | Importable |
|---|---|---|
| `cmd/redlite/` | The server binary (leader or replica by config) | — |
| `reader/` | The embeddable client: local map + WebSocket to the leader | **public** |
| `protocol/` | Wire message types shared by server and reader | **public** |
| `internal/cache` | Per-sheet store: TTL, jitter, null placeholders | — |
| `internal/sheet` | Sheet registry, collaborators, broadcast | — |
| `internal/lock` | Distributed locks: ownership, TTL, disconnect cleanup | — |
| `internal/ws` | WebSocket upgrade, action routing | — |
| `internal/replica` | Replica client, heartbeat, promotion | — |
| `internal/persist` | PostgreSQL write-behind, hydration, TTL cleaner | — |
| `internal/api` | HTTP management API (projects, keys, export/import) | — |
| `internal/config`, `internal/logger` | Config (env), slog logger | — |
| `db/migration/` | golang-migrate SQL files for the cache database | — |
| `doc/` | Scope, specs, decisions, protocol, persistence design, `reference/` (checked outside patterns) | — |
| `examples/` | Raw transcripts behind `doc/reference/`; inputs, never cited | — |
| `testing/` | Go integration harness + probe template + run.sh | — |

Anything under `internal/` is invisible to consumers by Go's rules. **A type a consumer must see
lives in `reader/` or `protocol/`, nowhere else.**

## Documentation

Lives in `/doc`. `doc/README.md` is the index.

1. **Read before you code.** Announce: *"Reading `doc/___.md` to understand current behavior."*
2. **Update when you change.** A new action, renamed field, changed message or removed behavior
   makes the doc wrong. Fix it in the same cycle. A doc that no longer matches the code is a bug.
3. **Create when missing.** New surface with no doc gets one, in the existing format.
4. Announce updates: *"Updating `doc/___.md` to reflect the changes made."*

## Workflow (mandatory)
- Before building: read doc/scope/SCOPE.md and doc/specs/<feature>.md. No spec → stop and ask.
- Plan Mode for anything touching more than one file.
- Search for existing code before creating new code. Search `advo_cache` for the reference shape, then the
  matching `doc/reference/` section for the checked pattern and its known defects; cite both.
- Never choose storage, failure behavior, eviction, batching, wire shape, auth, or TTL policy silently.
  Present recommendation + alternative + why it lost. Log the decision in doc/DECISIONS.md.
  Told to proceed without a decision → log it in doc/ASSUMPTIONS.md.
- Value-source gate: every value a message carries or the server computes has a ✅ row in the spec's value-source table.
  Any ❌ → stop and ask. Do not invent a source.
- **Wire compatibility.** `protocol/` changes are additive only: a new optional field or a new action is fine.
  Never rename, remove, or retype a field `advo_cache`'s reader or a shipped `backendAdvo` reads
  (`doc/protocol/wire.md` lists them). A breaking change → stop and ask; it needs a protocol version bump (D-id).
- Done = acceptance criteria verified against a running server (the integration test or a probe hit it), the
  race detector ran on the package touched, and the test for the changed area ran and passed (see Testing).
- Debug: reproduce first, one hypothesis at a time, revert failed attempts, fix the root cause.
  Session ending mid-bug → write doc/handoffs/<topic>_<YYYY-MM-DD>.md.
- After each feature: update SCOPE + spec status, doc/CHANGELOG.md, and this file/rules if conventions changed.

## Testing

- Evidence comes from test output, not from pasted logs or code-reading.
- **Unit tests** live beside the code (`internal/<pkg>/*_test.go`, `reader/*_test.go`). **Integration tests** live in
  `testing/integration/*_test.go`; each file's line 1 is `// Covers: <action or package>` — the counterpart of
  advomedix's `# Handler:` header. Find the covering test with `grep -rn "// Covers: <name>" testing/integration internal reader`.
- Run with the harness, never raw: `bash testing/run.sh <pkg>` (one package, e.g. `internal/cache`, `reader`, `integration`)
  or `bash testing/run.sh` (everything). It runs `go test -race -count=1` and writes `testing/runs/<timestamp>__<slug>.log`,
  `latest.log`, `latest.json`. Put its `Total | Passed | Failed` line and the relevant output in the report.
- No covering test, or it doesn't show what you need → /probe. Also write "NO TEST: <path>" in the report.
  Don't write a test file unless the track asks for one (FEATURE/CHANGE E, BUG repro).
- The integration harness boots its own leader (and replica when needed) on a free port with `httptest`; there is no
  dev server for Devin to run. Persistence tests need `REDLITE_TEST_DATABASE_URL`; when it is unset they skip and the
  report says `SKIPPED: persist (no REDLITE_TEST_DATABASE_URL)`. Never point a test at a production database.
- `go test ./...` **is allowed here** — nothing opens a real database without the env var above. Prefer `testing/run.sh`
  so the run is logged.
- `testing/runs/` is gitignored output; never commit it or list it as a deliverable.
- Any red line that isn't a KNOWN-DEFECT id → stop and report.

### Who runs which test
- The agent that changed the code runs the covering test for what it changed. Several agents: at most 4 at once,
  one per package, each with its own owned paths; the lead writes ONE brief (verified facts with file:line, owned
  paths, shared-file order) before launching; the lead runs the full `testing/run.sh` once per wave and compares it to the
  pre-wave baseline. One agent at a time in a shared file.
- The checker: on HIGH risk work and every multi-agent run, the `reviewer` subagent runs in a fresh context with only
  the spec's acceptance criteria and `git diff -- <owned paths>`. It runs the covering tests against each criterion
  first, then reviews the diff.
- Agents list their deletes; the lead runs them after the wave's full suite is green (## Delete policy).

## Delete policy
The one copy. Every other file points here and does not restate it.

Git stays read-only. A delete is one plain command: `rm <path> [<path> ...]` with literal absolute or repo-relative paths; no `cd` in the same command, no variables, no `find -delete`, no `xargs`, no shell `-c`, no script.
`.claude/hooks/rm-guard.sh` checks every delete. A blocked delete is final: never try another way; report it under LEFT FOR DEVIN with the guard's reason.
- **TEST OUTPUT, any time** (`rm -r` allowed, never `rm -rf`): `testing/runs`, `.DS_Store`, `*.swp`, `coverage.out`, `bin/`.
- **PROBES, when promoted or no longer needed:** `testing/_probe/<file>` (never `TEMPLATE.sh`, never `TEMPLATE_test.go`).
- **OBSOLETE FILES, after tests:** a tracked, unmodified NON-SOURCE file this request made obsolete (a folded doc, a
  fixed handoff, a one-off .md). grep proves nothing references it → `rm`, one file per path → `go build ./...` + the
  covering test → anything red: restore with `git show HEAD:<path>` and the Write tool, re-run, report NOT DELETED.
- **SOURCE FILES** (`.go .sql .sh` and the like) are never deleted by an agent. One this request made obsolete goes to
  LEFT FOR DEVIN with the grep that shows nothing references it.
- **NEVER, list for Devin:** untracked files outside the folders above, files with uncommitted edits, folders of source,
  `db/migration/**`, and everything the guard protects.
- Only what this request created or made obsolete. Never comment out or empty a file instead of deleting it. Never delete to make a test pass.
- Report every delete: DELETED (path | class | why) and LEFT FOR DEVIN (path | why).

## Logging
- One logger: `internal/logger` wraps `log/slog`. Package code calls `logger.L` (`*slog.Logger`). Never `fmt.Print*`,
  `log.Print*`, or a second logger. `cmd/redlite/main.go` is the only place that may print to stdout outside the logger
  (the startup banner), and it prints the banner through `logger.Banner`.
- Four levels: Debug dev-only tracing (cache hits, broadcast fan-out counts) · Info a prod-worthy state change (sheet
  opened, replica registered, promotion, flush completed) · Warn a handled failure (bad password, lock contention, drop
  on backpressure, reconnect) · Error unhandled or data-at-risk (panic recovered, persist write failed, hydration failed).
- One shape: `logger.L.Info("Sheet: opened", "sheet", id, "collaborators", n)` — message is `Module: event`, every value
  is a slog attribute, snake_case keys. Modules: Cache, Sheet, Lock, WS, Replica, Persist, API, Reader.
- **IDs only.** Never log a cached value, a password or its hash, an API key, a full key list, or a request/response
  body. A key may be logged (it is an identifier); its value never. Detail and WRONG/RIGHT pairs: `.claude/rules/backend/go-logging.md`.
- Every new or changed log line goes in the report: file:line | level | module | message.

## Context map
- `.claude/rules/backend/go-*.md` auto-load by path (frontmatter `paths:`).
- Skills in `.claude/skills/`: `/work <request>` — every task starts here · `/probe <action>` — runtime evidence when no test covers it · `/cleanup` — end of every track.
- `.claude/agents/reviewer.md` — the checker.

## Code standards
- DRY. One function, one location, multiple callers. No fallback copies.
- Plan architecture before implementation. Aim for O(1) space and time on the hot path (`Get`, `Set`, broadcast dispatch).
- A plain `map` + `sync.RWMutex` is the default concurrent map (`.claude/rules/backend/go-caching.md`). `sync.Map` only
  where a benchmark in the report justifies it. `advo_cache` used `sync.Map` everywhere; that is not a reason to here.
- Every goroutine takes a `context.Context` and exits on `ctx.Done()`. No goroutine-per-entry expiry. No bare `go func()` in a handler.
- Errors are wrapped with `%w` and carry the module: `fmt.Errorf("sheet: open %q: %w", id, err)`. Sentinel errors are exported from the package that owns them.
- `protocol/` structs use explicit `json:"..."` tags; omitted optional fields are `omitempty`. No `map[string]any` as a message type.
- Config is read once in `internal/config` from `REDLITE_*` env vars (`.env.example` is the list). Nothing else reads `os.Getenv`.
- Comments describe what the code actually does. We make changes based on them.
- Do only what is asked. No extra changes beyond the request.
- No testing until all actions of the current step are complete; then run the test for the changed area (see Testing).
- Never claim done without verifying.

## Self-check each cycle
- Am I solving the right problem? → DEFINE
- Did I read the docs, the `advo_cache` reference and the `doc/reference/` section before starting? → read them now
- Did I cite the origin (and the change) on every shape I borrowed? → add the comment
- Did I consider multiple approaches before coding? → HYPOTHESIZE
- Am I changing one thing at a time, and not stacking failed fixes? → reset, implement clean
- Do the docs still match the code? → update now
- Is the type I just added in the right package (`internal/` vs `reader/`/`protocol/`)?
- Did I change a wire field a shipped reader depends on? → stop
- Did the covering test or a probe run before I decided, and is its Total | Passed | Failed line in the report? No test → did I write NO TEST: <path>? Is every probe promoted or deleted, and is every delete reported?
- Did every new or changed log line follow ## Logging, and is it listed in the report?
