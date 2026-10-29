# Decisions

One entry per decision that was actually made — storage, eviction, batching, failure behavior,
wire shape, auth, TTL policy, or anything else that was not obvious. Newest at the bottom.
Decisions made without being decided belong in `ASSUMPTIONS.md`, not here.

Format:

```
## D-001 <title> (YYYY-MM-DD, accepted)

**Context**
What forced a choice.

**Chosen**
What we do, specifically.

**Alternatives + why lost**
- <alternative> — why it lost

**Consequences**
What this commits us to, and what it costs.
```

---

## D-000 Rebuild, not fork (2026-10-29, accepted)

**Context**
`advo_cache` is a working service with eight known defects (SCOPE.md K1–K8), `go 1.21`, Viper, a
hand-rolled logger, and `sync.Map` everywhere. It lives inside the advomedix monorepo and is not a
pinnable dependency.

**Chosen**
A new module at `/Users/devinanderson/Desktop/red_lite_cache`, Go 1.27, built feature by feature
from the advo_cache reference with each defect fixed by design. The wire protocol stays
v1-compatible so a reader swap in `backendAdvo` is the only consumer change.

**Alternatives + why lost**
- Fork and patch advo_cache in place — carries every defect's shape forward; keeps it unpinnable.
- Adopt Redis — a second runtime to operate on the NUC and the DO box; loses the embedded O(1) reader.

**Consequences**
Everything is rebuilt and must be re-tested. The reference is read-only. Pending: D-001 to D-010 in SCOPE.md.

## D-001 Module path and repository (2026-10-29, accepted)

**Context**
Every import path and the consumer pin in backendAdvo depend on it.

**Chosen**
`github.com/CrimsonCoder42/red_lite_cache`, public repo, default branch `main`. First tag `v0.0.1` (S0 scaffold, no behavior).

**Alternatives + why lost**
- Private repo: needs GOPRIVATE + credentials on every machine and CI that builds backendAdvo. Raw transcripts in `examples/` are kept out of git instead.

**Consequences**
Anything in git is public. `examples/*.md` stays untracked; never commit secrets or transcripts.
