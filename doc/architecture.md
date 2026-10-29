# Architecture

_Verified against: nothing yet (S0). Rewrite this file in the DOCS track once S2 ships._

## Processes

```
                 ┌──────────────── leader (cmd/redlite) ────────────────┐
  reader ──ws──▶ │ internal/ws ─▶ internal/sheet ─▶ internal/cache      │
  reader ──ws──▶ │      │              │                                 │
  reader ──ws──▶ │      │              └─▶ broadcast to collaborators    │
                 │      ├─▶ internal/lock                                │
                 │      └─▶ internal/persist ─▶ PostgreSQL (write-behind)│
                 │ internal/api  (HTTP: projects, keys, export)          │
                 └───────────────────────┬────────────────────────────────┘
                                         │ heartbeat + replicate_*
                 ┌──────────────── replica (cmd/redlite) ────────────────┐
                 │ internal/replica ─▶ internal/sheet (mirror)           │
                 │ promotes to leader on heartbeat timeout               │
                 └────────────────────────────────────────────────────────┘
```

## Goroutines per process (target)

| Goroutine | Owner | Exits on |
|---|---|---|
| one per WS connection (read loop) | `internal/ws` | socket close / ctx |
| one per WS connection (write pump) | `internal/ws` | socket close / ctx |
| cache janitor (one per process, not per sheet) | `internal/cache` | ctx |
| lock cleanup | `internal/lock` | ctx |
| heartbeat (leader) / heartbeat monitor (replica) | `internal/replica` | ctx |
| persist writer | `internal/persist` | ctx, after final flush |
| persist TTL cleaner | `internal/persist` | ctx |

Everything takes a `context.Context`; `cmd/redlite` cancels it on SIGTERM and waits with a deadline.

## Package dependency rule

`protocol` ← `internal/cache` ← `internal/sheet` ← `internal/lock` ← `internal/ws` ← `cmd/redlite`.
`reader` imports only `protocol`. `internal/persist` imports `internal/cache` (for the op types), never `ws`.
No package imports `cmd`. `go list -deps` is the check.
