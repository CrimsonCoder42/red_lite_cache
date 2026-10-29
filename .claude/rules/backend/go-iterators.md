---
paths:
  - "internal/cache/**/*.go"
  - "internal/persist/**/*.go"
---

# Iterators and Generators

_Origin: code-heim go_56_generator (channel generator, dated) and go_60_iter (range-over-func, current) — `doc/reference/codeheim_patterns.md` § Tier 2._

Producing a sequence one value at a time, without materializing the whole thing. Since Go 1.23 this is a language feature. Do not hand-roll it.

## Default: range-over-func

```go
// ✓ RIGHT — no goroutine, no channel, nothing to leak
func Primes() iter.Seq[int] {
    return func(yield func(int) bool) {
        for n := 2; ; n++ {
            if isPrime(n) {
                if !yield(n) {
                    return
                }
            }
        }
    }
}

for p := range Primes() {
    fmt.Println(p)
    if p > 100 {
        break   // becomes yield() == false; the iterator returns
    }
}
```

The consumer's `break` is compiled into `yield` returning `false`. Cleanup is automatic. The compiler inlines these, so they perform comparably to a hand-written loop.

```go
// ✗ WRONG — pre-1.23 workaround: costs a goroutine, a channel send per value,
//           and leaks if the consumer stops without signaling done
func Primes(done <-chan bool) <-chan int {
    ch := make(chan int)
    go func() {
        defer close(ch)
        for n := 2; ; n++ {
            select {
            case <-done:
                return
            case ch <- n:
            }
        }
    }()
    return ch
}
```

The `done` channel is not a fix, it is a requirement placed on every caller. Forget to signal it and the goroutine blocks on send forever.

## Always check yield's return value

```go
// ✗ WRONG — panics: "range function continued iteration after
//           function for loop body returned false"
yield(v)
continue

// ✓ RIGHT
if !yield(v) {
    return
}
```

## Errors go through Seq2

```go
func ParseAll(lines []string) iter.Seq2[int, error] {
    return func(yield func(int, error) bool) {
        for _, l := range lines {
            n, err := strconv.Atoi(l)
            if !yield(n, err) {
                return
            }
        }
    }
}
```

Never swallow an error inside the iterator. Never `defer`/`recover` inside the iterator body either: the caller sees a clean early finish and cannot tell the sequence was truncated.

## Cleanup

`defer` inside the iterator function runs when the iterator returns, which a consumer `break` triggers. That is the correct place to close a file or release a handle.

## When a channel is still right

Only when the producer must run concurrently with the consumer, doing real work in flight, such as prefetching pages while the consumer processes the current one. Iterators are synchronous and single-goroutine by design.

Needing values lazily is not by itself a reason for a goroutine.

## Pull style

`iter.Pull` converts a push iterator into `next()` / `stop()` when you need to interleave two sequences or advance manually. Call `stop()`, and prefer `defer stop()` at the point you obtain it.