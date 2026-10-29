---
name: probe
description: Write and run a temporary probe against a local red_lite_cache server to see a message, shape or status when no test covers it. Use when a QUESTION or BUG needs runtime evidence.
---

# Probe

Probe target: $ARGUMENTS

Evidence comes from test output, not from pasted logs or from reading code alone.

## First: is there already a covering test?

`grep -rn "// Covers: <name>" testing/integration internal reader`. If one covers the question, run
it instead: `bash testing/run.sh <pkg>`. Put its `Total | Passed | Failed` line and the relevant
output in the report. Only write a probe when no test covers it, or the test doesn't show what you need.

## Two probe shapes

**Go probe (preferred).** Copy `testing/_probe/TEMPLATE_test.go` to
`testing/_probe/<topic>_<date>_test.go`. It boots a leader with `integration.StartLeader(t)`, opens a
sheet with `reader.New`, and prints what it sees with `t.Logf`. Run it with
`go test -race -count=1 -v ./testing/_probe/ -run <Name>`. **Assert nothing you don't yet know.** A
probe observes; it does not verify.

**Shell probe.** Copy `testing/_probe/TEMPLATE.sh` when the question is about raw frames (what the
server sends on the wire). It needs `websocat` and a server you started with
`REDLITE_PORT=:0 go run ./cmd/redlite` — the banner prints the chosen port. Stop the server when done.

Paste the output. Decide from the output.

## Lifecycle

A probe is temporary. When the work ships:

- Checks something a reader relies on → **promote** it into the covering test file (unit beside the
  code, or `testing/integration/` with a `// Covers:` line 1).
- Otherwise → **delete** it under `.claude/CLAUDE.md` → "## Delete policy" (PROBES) and report it as DELETED.

No probe survives `/cleanup`. `testing/_probe/.gitignore` keeps probes uncommittable (the two
TEMPLATE files are the exception), so an un-promoted probe is lost, not shipped.

## Before you probe persistence

A persist probe writes to whatever `REDLITE_TEST_DATABASE_URL` points at. Say so first, and never
point it at a database that is not a local test database.
