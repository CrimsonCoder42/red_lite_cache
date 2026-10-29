# Changelog

- 2026-10-29 — advomedix-v2 read-only: `.claude/hooks/advomedix-readonly.sh` (PreToolUse Bash|Edit|Write|MultiEdit|NotebookEdit; 23 cases checked: 7 allow, 16 block), `settings.json` deny Edit/Write/NotebookEdit + secret reads on advo_production/**, `settings.local.json` `additionalDirectories` + Read allows for advo_cache, backendAdvo, .gitea/workflows, doc/backend. `rm`/`rmdir`/`unlink` moved from allow to deny (policy is `mv -n` to `_delete/`).

- 2026-10-29 — purpose + agent prompt: `doc/purpose.md` (backendAdvo usage researched: call-site counts, 3 instances on one
  sheet, R1–R12 from `backendAdvo/cache/store.go:8-87`); `.claude/prompts/request.md` (request header adapted from the
  advomedix one: dependency + migration checkpoints replace SQLC, CONTRACTS replace the uniform contract, seven
  integration loops replace CRUD/UI loops). `doc/protocol/wire.md`: `ttl` (minutes) added to `set` and the
  compatibility list — it was missing. Delete policy switched to move-to-`_delete/` in CLAUDE.md to match.

- 2026-10-29 — published: github.com/CrimsonCoder42/red_lite_cache (public), `main` @ 71534ec, tag v0.0.1. D-001 accepted.

- 2026-10-29 — reference knowledge: `examples/` (4 raw transcripts, Devin) indexed by `examples/README.md`;
  `doc/reference/codeheim_patterns.md` (46 code-heim repos cloned and read; two found not to compile:
  go_82 both servers miss `import "sync"`); `doc/reference/distributed_cache.md` (Anthony GG stream
  distilled, claims checked); `doc/reference/README.md`. `.claude/CLAUDE.md` "Reference sources" (three
  layers, cite-the-origin rule), `/work` Phase 0/C/D read and cite references, reviewer dimension 6b
  provenance, `/cleanup` §6 fixes a wrong reference row, every `rules/backend/*.md` carries an Origin line.
  SCOPE: D-011 (websocket library) added to pending decisions.

- 2026-10-29 — scaffold (S0): `.claude/` (CLAUDE.md, settings, hooks, rules, skills, reviewer), `doc/`
  (scope, specs template, decisions D-000, assumptions, protocol, persist, architecture, release),
  `testing/` (run.sh, integration harness stub, probe templates), `go.mod` Go 1.27, package stubs that
  build, `cmd/redlite` health server, `.env.example`, Dockerfile, docker-compose. No behavior yet.
