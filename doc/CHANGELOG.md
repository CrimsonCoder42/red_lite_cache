# Changelog

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
