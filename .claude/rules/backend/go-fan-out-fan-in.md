---
paths:
  - "internal/sheet/**/*.go"
  - "internal/ws/**/*.go"
---

# Fan-Out / Fan-In

_Origin: code-heim go_25_fan_out_fan_in, go_61_err_group, go_67_conc (`doc/reference/codeheim_patterns.md` § Tier 2). Unbounded goroutine-per-item there; bounded with errgroup.SetLimit here._

Split independent work across goroutines, collect results on a channel. Applies when one stage of a pipeline dominates the runtime and its units are independent.

## Bound the fan-out

One goroutine per item is the default mistake. It works on four items and OOMs on fifty thousand, because concurrency scales linearly with input size.

Use `errgroup` with `SetLimit`. It is the semaphore and the fan-out in one call: `g.Go` blocks once the limit is reached.

```go
g, ctx := errgroup.WithContext(ctx)
g.SetLimit(limit)
for i, item := range items {
    g.Go(func() error {
        r, err := process(ctx, item)
        if err != nil {
            return fmt.Errorf("process %v: %w", item, err)
        }
        results[i] = r
        return nil
    })
}
if err := g.Wait(); err != nil {
    return nil, err
}
```

Writing to distinct indices of a preallocated slice needs no mutex and no collection channel.

## Pick the limit from the workload

- CPU-bound (image resize, compression, hashing): `runtime.NumCPU()`. More goroutines than cores adds scheduling overhead and memory, not throughput.
- IO-bound (network, disk): higher, capped by what the downstream can absorb.

Never leave it as `len(items)`.

## Errors must propagate

A goroutine that drops its error makes the failure invisible. `errgroup.WithContext` captures the first non-nil error and cancels the shared context so the remaining workers stop instead of finishing work that will be discarded.

If a plain channel is used instead, the result struct carries an `error` field and the collector checks it.

- A recovered panic is logged at Error with the module name. Never a bare `recover()`.

## Close the channel, do not count

Consuming exactly N results assumes every producer succeeds. One early return and the collector blocks forever.

```go
var wg sync.WaitGroup
for _, item := range items {
    wg.Add(1)
    go func() { defer wg.Done(); out <- process(item) }()
}
go func() { wg.Wait(); close(out) }()

for r := range out { ... }
```

The consumer terminates on close, not on arithmetic.

## Buffer for throughput, not for the whole batch

A channel buffered to `len(items)` holds every result in memory at once. With image or document payloads that is the memory blowup the fan-out was supposed to avoid. Size the buffer to the concurrency limit and let the next stage drain it.

## Loop variables no longer need copying

Since Go 1.22 loop variables are per-iteration scoped. `item := item` at the top of the body, and passing the value as a goroutine parameter purely to capture it, are both obsolete. Do not add them; do not flag their absence as a bug.

Passing a parameter for any other reason is still fine.