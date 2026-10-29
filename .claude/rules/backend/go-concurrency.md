---
paths:
  - "internal/**/*.go"
  - "reader/**/*.go"
  - "cmd/**/*.go"
---

# Concurrency

_Origin: code-heim go_20_mutexes_and_confinement, go_18_worker_pool, go_51_context (`doc/reference/codeheim_patterns.md` § Tier 2). None of them take a ctx; every owner goroutine here does._

Fan-out across independent work is covered in `go-fan-out-fan-in.md`. This file covers the request path and shared state.

## Request handling is already concurrent

`net/http` spawns a goroutine per incoming request in `server.Serve`. Gin runs on top of `net/http` and inherits this. One hundred simultaneous requests means one hundred goroutines, with no work from us.

Do not add concurrency to make request handling parallel. It already is.

## Never spawn goroutines in a handler for long-running work

Sending email, generating a report, processing an upload, calling a slow third party: none of these get a bare `go func()` in a handler.

An orphaned goroutine gives no retry, no monitoring, no backpressure, and no way to know it failed. It dies silently with the process on deploy.

Offload to the background system instead. Worker pools and pipelines belong there, where throughput and resource limits are the actual problem.

## Any value touched by two goroutines needs synchronization

An unsynchronized shared variable does not fail loudly. It produces wrong numbers that look plausible: a counter guarded by nothing will read a stale value, decrement it, and write back over another goroutine's write. The bug is intermittent and load-dependent.

Two correct answers, in order of preference:

**Confinement.** Give the value a single owning goroutine and let others send requests over a channel. Nothing else touches it, so nothing can race. Use `select` to handle both work and a shutdown signal so the owner exits cleanly rather than leaking.

**Mutex.** When confinement is impractical, `sync.Mutex` around the critical section.

- `defer mu.Unlock()` runs at function exit, so it holds the lock across everything after it. If the function then does IO or calls out to another service, that lock is held for the whole call. Scope the critical section to the smallest block that touches shared state.
- `sync.RWMutex` when reads heavily outnumber writes.
- `sync/atomic` for a bare counter or flag. A mutex around a single increment is overkill.

## Prove it with the race detector

Reasoning about a race is not evidence. Run `-race`.

```
go run -race .
```

Any code introducing shared state gets `go test -race ./<package>/...` on the package touched before it is called done,
once that package's tests compile. Never `./...` (see CLAUDE.md Testing). If the package has no compiling tests, say so in the report.

## WaitGroups

Use `wg.Go(func(){ ... })`. Added in Go 1.25, it does the `Add(1)`, the goroutine, and the `defer Done()` in one call, which removes the mismatch bugs entirely.

The old form is still correct where it already exists:

```go
wg.Add(1)
go func() {
    defer wg.Done()
    ...
}()
```

- `Add` goes before the goroutine starts, never inside it. Calling it inside is a race that lets `Wait` return early. `go vet` flags this as of 1.25.
- A missing `Done` deadlocks `Wait`. An extra one panics with a negative counter.
- Never copy a `WaitGroup`. Pass a pointer. `go vet` catches this as `copylocks`.
- A recovered panic is logged at Error with the module name. Never a bare `recover()`.

## Before adding any concurrency primitive

1. Is this the request path? Then it is already concurrent. Stop.
2. Is this long-running or failure-prone? Then it belongs in a queued job, not a goroutine.
3. Is this shared state? Then confine it or lock it, and prove it with `-race`.
4. Is this inside a worker, bounded, and monitored? Then a pool or pipeline is appropriate.

If none apply, the concurrency is not justified.