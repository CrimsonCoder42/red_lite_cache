---
name: cleanup
description: End-of-work cleanup: fold specs/handoffs/audits into doc/, update DECISIONS/ASSUMPTIONS/SCOPE/CHANGELOG, promote or delete probes, delete what the work made obsolete under the delete policy, and flag stale .claude/ text. Use after any track ships.
---

# Cleanup

Scope: $ARGUMENTS

Run this after a track ships, before the work is called done.

## 1. Fold the working notes into `doc/`

- A spec that shipped → update its status in `doc/specs/<feature>.md` and in `doc/scope/SCOPE.md`.
- A handoff written mid-bug (`doc/handoffs/<topic>_<YYYY-MM-DD>.md`) → fold its conclusion into the
  area doc and say the handoff is now stale.
- A protocol change → `doc/protocol/wire.md` lists the new field or action with the version it arrived in.
- An audit or report produced as a one-off → its findings belong under `doc/`, not in a root-level `.md`.

## 2. Update the ledgers

- `doc/CHANGELOG.md` — one entry: what changed, `file:line` for each claim, the test line.
- `doc/DECISIONS.md` — any decision taken on storage, eviction, batching, failure behavior, wire
  shape, auth or TTL policy, with the alternative and why it lost.
- `doc/ASSUMPTIONS.md` — anything you were told to proceed on without a decision.
- `doc/scope/SCOPE.md` + spec status — the slice and its state.
- `.claude/CLAUDE.md` / rules — only if a convention actually changed.

## 3. Probes

Every `testing/_probe/` file from this work is either promoted into a covering test or deleted
under the delete policy. No probe survives cleanup.

## 4. Deletes

Delete what this work made obsolete — superseded handoffs, test output, one-off reports, un-promoted
probes — exactly as `.claude/CLAUDE.md` → "## Delete policy" says. In a multi-agent run, list the
deletes for the lead instead.

Report `DELETED (path | class | why)` and `LEFT FOR DEVIN (path | why)`.

## 5. Sync

List any `CLAUDE.md`, rule, or skill line the shipped diff made false:

```
file:line | says | now
```

A context file that no longer matches the code is a bug, not a nit.

## 6. References

If the work showed a `doc/reference/` claim to be wrong, dated, or missing a defect, fix that row in the
reference (with the test or output that showed it) in the same cleanup. A reference that disagrees with
shipped code is a doc bug like any other.

## 7. Release readiness (when the track touched `reader/` or `protocol/`)

State whether the change is additive. If yes, note the next tag to cut (`vX.Y.Z`) in the CHANGELOG
entry so Devin can tag it; `backendAdvo` pins a tag, not `main`.
