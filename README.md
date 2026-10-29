# red_lite_cache

A multi-tenant WebSocket cache with an embeddable Go reader, leader/replica replication,
distributed locks, and PostgreSQL write-behind persistence. The successor to `advo_cache`.

**Status:** scaffold (S0). No behavior yet. `doc/scope/SCOPE.md` is the plan.

```go
import "github.com/CrimsonCoder42/red_lite_cache/reader"
```

- Docs: `doc/README.md`
- Wire protocol: `doc/protocol/wire.md`
- Releasing and pinning: `doc/release.md`
- Tests: `bash testing/run.sh`
