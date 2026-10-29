---
paths:
  - "internal/**/*.go"
  - "reader/**/*.go"
  - "cmd/**/*.go"
---

# Logging

_Origin: code-heim go_26_slog (ReplaceAttr masking, child loggers) replaces go_19_custom_logger and advo_cache `logger/logger.go` — `doc/reference/codeheim_patterns.md` § Tier 3._

One logger: `logger.L` (`internal/logger`, a `*slog.Logger`). Never `fmt.Print*` or `log.Print*` in
package code; the hook rejects it. The reader is a library a consumer embeds, so `reader.Config` takes an
optional `*slog.Logger`; when nil it uses `slog.Default()`. The reader never writes to stdout on its own.

**Levels.** `Debug` dev-only tracing: cache hit/miss, broadcast fan-out counts, flush sizes · `Info` a
prod-worthy state change: sheet opened or dropped, replica registered, promotion, hydration done, flush
done · `Warn` a handled failure: bad password, lock contention, enqueue dropped on backpressure, reconnect,
stale replica · `Error` unhandled or data-at-risk: panic recovered, persist write failed, hydration
failed, listener closed with an error.

**Shape.** The message is `Module: event`; everything else is an attribute with a snake_case key.

```go
logger.L.Info("Sheet: opened", "sheet", id, "collaborators", n)
logger.L.Warn("Persist: enqueue dropped", "sheet", id, "queue_len", len(q), "cap", cap(q))
logger.L.Error("Persist: flush failed", "batch", len(b), "err", err)
```

Modules: Cache, Sheet, Lock, WS, Replica, Persist, API, Reader, Config.

**Identifiers only.**

```go
logger.L.Info("Cache: set", "key", k, "value", v)          // ✗ WRONG — the value is tenant data
logger.L.Debug("Cache: set", "key", k, "ttl_s", ttl.Seconds()) // ✓ RIGHT — the key finds the row

logger.L.Warn("Sheet: bad password", "sheet", id, "got", pw) // ✗ WRONG — a credential in stdout
logger.L.Warn("Sheet: bad password", "sheet", id, "remote", r.RemoteAddr) // ✓ RIGHT

logger.L.Info("API: key issued", "project", p, "key", key)   // ✗ WRONG
logger.L.Info("API: key issued", "project", p, "key_prefix", key[:8]) // ✓ RIGHT — traceable, not usable
```

Never log: a cached value, a password or bcrypt hash, an API key, a full key dump, a message body, a
`DATABASE_URL`. A key name is fine; its value is not.

**Log once at the boundary.** The WS action router logs a failed action at Warn with the action and
error; the handler it dispatched to does not log the same error first. A recovered panic is always
logged at Error with its module and `recover()` value. Never a bare `recover()`.

**Startup banner.** `cmd/redlite/main.go` prints the role, port, and leader address through
`logger.Banner`. The 150-line how-to that `advo_cache/main.go:95-260` printed on every boot belongs in
`doc/protocol/wire.md`, not stdout.
