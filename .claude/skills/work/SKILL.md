---
name: work
description: "Run any red_lite_cache request through the engineering workflow: orient, gather test evidence, classify the track (FEATURE/CHANGE/BUG/REFACTOR/DOCS/QUESTION), then execute it with stop points. Use for every request that touches code or docs."
disable-model-invocation: true
---

You are a senior Go engineer working inside the red_lite_cache engineering system.
The code standards, Testing and Logging rules in .claude/CLAUDE.md apply to every line below.
Don't restate them. Follow them. Repo root: /Users/devinanderson/Desktop/red_lite_cache
Reference (read-only): /Users/devinanderson/Desktop/advo_production/advomedix-v2/advo_cache

Request: $ARGUMENTS

═══ PHASE 0 — ORIENT (read-only) ═══
1. Read doc/scope/SCOPE.md, doc/DECISIONS.md, doc/ASSUMPTIONS.md, doc/protocol/wire.md, and every
   doc/specs/ and doc/handoffs/ file that mentions this area. Read the advo_cache file(s) that
   implement the same feature and note file:line. Read the doc/reference/ section(s) for the
   pattern (codeheim_patterns.md by repo, distributed_cache.md by component) and note which
   "claims checked" verdicts and defects apply. Sources there are 2023–2025 / Go 1.18–1.23: say
   what has changed since (Go 1.27, current library tag) before relying on a shape.
2. TESTS — find the covering test: grep -rn "// Covers: <name>" testing/integration internal reader.
   State: file(s) | what they assert | last known result (testing/runs/latest.json). None → "NO TEST for <pkg>".
3. Classify as exactly one: FEATURE (new behavior) · CHANGE (existing behavior) · BUG ·
   REFACTOR/PERF (no behavior change) · DOCS · QUESTION.
4. Risk: LOW (docs, banner, comments) · NORMAL · HIGH (persistence, replication/promotion, locks,
   protocol/, auth or API keys, anything with shared state).
5. Announce, then wait: "Class: ___. Risk: ___. Packages: ___. Tests: <files> or NO TEST.
   Reference: advo_cache/<file>:<lines> · doc/reference/<file>.md § <section> (verdicts that apply:
   ___; defects to avoid: ___). Existing state: spec yes/no · decisions that apply ·
   open assumptions · handoffs. Steps I will run: ___." Unclear class → ask before announcing.
⏸ = stop and wait for me.

═══ PHASE 1 — EVIDENCE (every track except DOCS) ═══
Run the covering test: bash testing/run.sh <pkg>. Put its "Total | Passed | Failed" line and the
output that matters in the report. If no test shows what the request needs (a message shape, which
side set a value, what a reader receives) → /probe. Decide from output, not from reading code alone.
⏸ if the evidence changes the class or plan.

═══ FEATURE / CHANGE ═══
A. SCOPE — skip if SCOPE.md has the slice. Else interview me (AskUserQuestion, ≤4 per round):
   consumers, v1 in/out, dependencies, packages. Add the slice row. ⏸
B. SPEC — doc/specs/<feature>.md from doc/specs/TEMPLATE.md: goal, numbered acceptance criteria,
   actions/messages/packages, value-source table, failure behavior, reuse list, out of scope, and
   TESTS: the test file each criterion will be asserted in. Search the code and advo_cache before
   the reuse list. Every load-bearing decision (storage, eviction, batching, failure behavior, wire
   shape, auth, TTL policy): recommendation + alternative + why it lost → ask me → D-### in
   DECISIONS.md. Any value-source row ❌ → ask me. Wire changes are additive only; a breaking one → ask me.
C. PLAN — plan mode: files to change (absolute paths), code reused, the advo_cache file and the
   doc/reference/ section each new piece is modeled on and what changes from it, protected/untouched,
   section order, the test command per criterion.
   LOW risk: B and C in one message. ⏸ approve
D. BUILD — section by section. Every borrowed shape carries one origin comment
   (`// after code-heim go_NN_x / advo_cache/<file>:<line> / distributed_cache.md § N: <what changed>`);
   the report lists each with why it changed. Comments describe what the code does now.
   Log lines follow ## Logging and are listed in the report. Hooks run on every edit; fix what they
   report. Then run the covering test for what you changed (bash testing/run.sh <pkg>); a red line
   that isn't a KNOWN-DEFECT id → stop. Mark progress in the spec. A decision the spec doesn't
   cover → stop and ask; if I say proceed → ASSUMPTIONS.md.
E. TEST — each criterion becomes a test: unit beside the code, or testing/integration/<feature>_test.go
   with line 1 "// Covers: <name>". KEEP a probe when it asserts a shape a reader reads, reproduces a
   shipped bug, proves a criterion, or crosses a sheet/replica/auth boundary; otherwise it is TEMP and
   is deleted (Delete policy, PROBES). Run with -race; paste the line.
F. VERIFY — each criterion against the running system: the test from E, or a probe. Pass/fail per
   criterion. HIGH risk or any multi-agent run: the reviewer subagent checks it (fresh context,
   acceptance criteria + `git diff -- <owned paths>`); fix what it finds. ⏸ I test
G. SHIP — after I confirm: doc/ matches the code (wire.md for any protocol change), CHANGELOG from
   git diff, SCOPE + spec status, stale comments fixed, stale CLAUDE.md/rules/skills listed.
H. /cleanup

═══ BUG ═══
doc/handoffs/ first. Reproduce = a failing assertion (extend the covering test or /probe); paste it.
Narrow to the smallest failing spot → 3–4 hypotheses → test ONE → revert if wrong → root cause,
not symptom → same pattern elsewhere (and in advo_cache: note it, don't fix it there) → the assertion
passes; paste the line. The assertion is KEPT. Scope guard: touch only what the failing assertion
needs. Anything else found → "FOUND, NOT FIXED: file:line | why" in the report + a SCOPE.md row.
A fix that needs a decision nobody made is a CHANGE: stop and reclassify. Design flaw → stop and
tell me. Then SHIP + /cleanup. Unfinished → doc/handoffs/<topic>_<YYYY-MM-DD>.md with the output.

═══ REFACTOR / PERF ═══
Baseline first (benchmark with go test -bench on the package, allocation count, covering-test pass
count). Each step shippable alone, behavior unchanged, re-measured, covering test green, -race
green. Then SHIP + /cleanup.

═══ DOCS ═══
Code and test output are the truth. Read all code for the area, run the covering tests, rewrite
doc/<area>.md with file:line and a Validation table (old claim | what the code does | test that
shows it). Report comment mismatches without fixing. Unknown → "Unknown — ask Devin", then ask.
Replaced one-off files are deleted under the Delete policy (OBSOLETE FILES).

═══ QUESTION ═══
Answer from the code with file:line. Runtime questions (what a message contains, what a reader
sees, which side set a value) → covering test or /probe; paste the output. No edits except the
probe. Reveals a bug or a missing doc → say so, offer the track. Probe → deleted (Delete policy).

═══ ALWAYS ═══
- Absolute paths in every report. Print the path before each write.
- Never touch db/migration, .env, vendor/, deploy/, .github/. Never run docker, migrate, psql, go get,
  go mod vendor, git writes, or bash -c / sh -c / python3 wrappers. mv -n only. Deletes follow
  .claude/CLAUDE.md "## Delete policy" exactly; anything it doesn't allow goes under LEFT FOR DEVIN.
- advo_cache and everything under advomedix-v2 are read-only references.
- A new dependency is a decision: name it, the alternative, and why; ask before adding to go.mod.
- Several agents: follow CLAUDE.md "Who runs which test".
- Ask, don't assume. One decision at a time.
- Every claim about code cites file:line; every claim about behavior cites a test or probe line.
  Where a doc disagrees with the code, the code wins; note it.
- go vet clean after every step (the hook enforces it).
- Reports ≤40 lines: per-step status, test lines, decisions needed, DELETED (path | class | why)
  and LEFT FOR DEVIN (path | why). Never restate files, rules or docs I already have.

═══ THE LOOP ═══

```
DEFINE → HYPOTHESIZE → BUILD & TEST → ANALYZE → DECISION POINT
   ↑                                                 |
   └──── Redefine / Pivot / Refine ←─────────────────┘
```

**1. DEFINE.** State the problem in one sentence and what "done" measurably looks like. Read the
doc for the area and the advo_cache reference. Identify the packages, types and functions involved.
Announce: *"The problem is ___. Success looks like ___. The relevant code lives in ___. The reference is ___."*

**2. HYPOTHESIZE.** List 3-4 approaches explicitly. Pick the most likely. State it: *"If I do X,
then Y should happen because Z."* Plan the package boundary before the code.

**3. BUILD & TEST.** Section by section, never one-shot. Announce: *"Let's do section X now."*
Smallest change that tests the hypothesis. One thing at a time.

**4. ANALYZE.** Did it meet the criteria? Did -race stay green? Do the docs still describe actual
behavior? If stuck: *"Let's step back and examine why this is failing."* Add a Debug log line to
surface reality before guessing.

**5. DECISION POINT.** Announce the path explicitly.

| Result | Action | Announce |
|---|---|---|
| Passes all criteria | Clean up, finalize, update plan and docs | "This section is complete. Updating plan and docs." |
| Partial progress | Refine implementation | "Partial progress. Refining section X." |
| Wrong approach | Drop it, re-hypothesize from what you learned | "This approach isn't working. Stepping back to re-hypothesize." |
| Wrong problem | Redefine | "I was solving the wrong problem. Redefining." |

**Loop discipline.** Regenerating similar code repeatedly means stop and re-hypothesize. A fix found
on attempt four gets implemented clean, never layered onto three failed attempts.
