# <Feature> — Spec

**Status:** draft | approved | built | verified
**Reference:** `advo_cache/<file>:<lines>` (what this rebuilds), or "new"

## Goal

_One sentence. What this does and for which consumer._

## Acceptance criteria

1.
2.
3.

Each one must be verifiable against a running server or a `-race` test, not against the code.

## Surface

| Kind | Name | Package |
|---|---|---|
| action / message / func |  |  |

## Value-source table

Every value a message carries or the server computes gets a row. A row is ✅ only when the source
is real and named. Any ❌ → stop and ask. Do not invent a source.

| Value | Source (field / config / computed from) | Decided |
|---|---|---|
|  |  | ❌ |

## Failure behavior

| Failure | What the caller sees | What the system does |
|---|---|---|
| Socket down |  |  |
| Sheet not open |  |  |
| Lock held |  |  |
| Persist unavailable |  |  |
| Replica lag |  |  |

## Reuse

_Existing types, funcs, helpers this uses instead of rebuilding. Name them with paths._

## Decisions

_D-ids from doc/DECISIONS.md that govern this feature._

## Wire compatibility

_Additive? List every new field/action. If anything a v1 reader reads changes: stop._

## Out of scope

## Tests

| Criterion | Test file | Kind |
|---|---|---|
| 1 |  | unit / integration |
