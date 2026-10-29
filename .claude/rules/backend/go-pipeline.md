---
paths:
  - "internal/persist/**/*.go"
  - "internal/replica/**/*.go"
---

# Pipeline

_Origin: code-heim go_21_goroutines_pipeline and go_62_ordone_tee_channel (`doc/reference/codeheim_patterns.md` § Tier 2). The stage shape is theirs; the ctx select on every stage is the addition._

Stages connected by channels, each stage a goroutine transforming its input and sending downstream. Use when processing breaks into distinct sequential steps and stages can overlap in time.

## Every stage takes a context and selects on it

Closing a channel propagates forward. It does nothing backward. If the consumer stops early, every upstream stage blocks forever on send, and every one of those goroutines leaks.

```go
// ✗ WRONG — bare send; consumer stops early and this goroutine never exits
func resize(in <-chan Job) <-chan Job {
    out := make(chan Job)
    go func() {
        defer close(out)
        for j := range in {
            j.Image = imageprocessing.Resize(j.Image)
            out <- j
        }
    }()
    return out
}

// ✓ RIGHT
func resize(ctx context.Context, in <-chan Job) <-chan Job {
    out := make(chan Job)
    go func() {
        defer close(out)
        for j := range in {
            j.Image = imageprocessing.Resize(j.Image)
            select {
            case out <- j:
            case <-ctx.Done():
                return
            }
        }
    }()
    return out
}
```

Ranging over `in` handles the upstream close. The `select` handles the downstream abandon. Both are required.

## Close exactly once, in the goroutine that sends

`defer close(out)` at the top of the producing goroutine. Never close from the consumer side, never from a second goroutine. Closing a channel twice panics; sending on a closed channel panics.

## Errors travel in the struct, not the void

```go
// ✗ WRONG — read failure produces a zero-value image that flows through
//           every stage and gets written to disk
j.Image = imageprocessing.ReadImage(p)
```

```go
// ✓ RIGHT — carry it, and let later stages pass through untouched
type Job struct {
    InPath  string
    OutPath string
    Image   image.Image
    Err     error
}

for j := range in {
    if j.Err != nil {
        forward(j)   // do not process, do not overwrite
        continue
    }
    ...
}
```

A final stage that reports `true` unconditionally is reporting nothing.

## A pipeline is depth, not width

Stages overlap: image 3 is read while image 2 is resized and image 1 is written. That is the entire benefit, and it is bounded by the slowest stage. Total throughput never exceeds what that one stage can do.

If one stage dominates, the pipeline alone will not fix it. Fan that stage out across bounded workers and merge the results. See `go-fan-out-fan-in.md`.

## Buffer size

Unbuffered channels run the stages in lockstep, so a jittery stage stalls its neighbors. A small buffer absorbs that.

Size it to smooth variance, not to hold the batch. Each buffered slot holds a full payload; with decoded images that is the memory the streaming design was meant to avoid.

## Signature convention

```go
func stage(ctx context.Context, in <-chan T) <-chan U
```

Directional channel types in the signature. The stage owns its output channel and returns it as receive-only, so no caller can close it or send on it.