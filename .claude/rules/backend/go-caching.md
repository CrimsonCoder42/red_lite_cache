---
paths:
  - "internal/cache/**/*.go"
  - "internal/sheet/**/*.go"
  - "reader/**/*.go"
---

# Caching

_Origin: code-heim go_11_cache_sync_map and go_45_memcache (`doc/reference/codeheim_patterns.md` § Tier 1), advo_cache `cache/cache.go`. Both ship the goroutine-per-entry expiry and the `sync.Map` reflex this file forbids._

## Use the shared cache store

Server-side storage is `internal/cache`, one instance per sheet. The reader's local map is `reader/`. Do not stand up a third ad-hoc map beside either.

An in-process map is invisible to every other instance, resets on deploy, and gives each pod its own divergent view of the same data. It is a local memoization tool, not a cache layer.

## sync.Map is not the default concurrent map

Go's own documentation: the Map type is specialized, and most code should use a plain map with separate locking, for better type safety and to keep other invariants alongside the content.

It pays off in exactly two shapes:
1. Entries written once, read many, cache only grows.
2. Goroutines working on disjoint key sets.

A TTL cache is neither. Entries are written and deleted, and every goroutine touches the same hot keys. Use a struct with a plain map and an `RWMutex` unless a benchmark shows otherwise.

`sync.Map` also gives you no `len`, no iteration, and no type safety.

## Never spawn a goroutine per entry for expiry

```go
// ✗ WRONG — one sleeping goroutine per cache entry, none of them cancellable
cache.Store(key, val)
go func() {
    time.Sleep(5 * time.Minute)
    cache.Delete(key)
}()
```

Ten thousand cached entries is ten thousand goroutines parked in `time.Sleep`, each holding its key alive. They cannot be cancelled, and a re-store does not reset the existing timer.

```go
// ✓ RIGHT — expiry lives in the value; one janitor, or none at all
type entry struct {
    val       any
    expiresAt time.Time
}

func (c *Cache) Get(k string) (any, bool) {
    c.mu.RLock()
    e, ok := c.m[k]
    c.mu.RUnlock()
    if !ok || time.Now().After(e.expiresAt) {
        return nil, false
    }
    return e.val, true
}
```

Lazy expiry on read covers correctness. A single periodic janitor goroutine covers memory. Prefer the cache store's native TTL over either.

## Check-and-set must be atomic — use singleflight

A read-then-compute-then-store sequence is three operations, and two goroutines interleave
inside it. On a cold key hit by fifty simultaneous requests, the expensive path runs fifty
times. That is the stampede that takes the database down.

```go
// ✗ WRONG — two goroutines both miss, both compute, both store
if v, ok := c.Load(key); ok {
    return v
}
v := expensive(key)
c.Store(key, v)
```

```go
// ✓ RIGHT — one flight per key, every waiter gets that result
var g singleflight.Group

func (c *Cache) GetOrLoad(ctx context.Context, key string) (any, error) {
    if v, ok := c.Get(key); ok {
        return v, nil
    }
    v, err, _ := g.Do(key, func() (any, error) {
        // Re-check under the flight: an earlier waiter may have filled it.
        if v, ok := c.Get(key); ok {
            return v, nil
        }
        v, err := loadFromDB(ctx, key)
        if err != nil {
            return nil, err
        }
        c.Set(key, v)
        return v, nil
    })
    return v, err
}
```

`golang.org/x/sync/singleflight` runs the function once per key and hands the result to
every waiter. This is the default for anything backed by a query, an HTTP call, or a
computation over a result set — which is nearly everything cached here.

Use `g.Forget(key)` if a failed load must not be shared by later callers, and prefer
`g.DoChan` with a `select` on `ctx.Done()` when the caller must be able to walk away.

## LoadOrStore — cheap values only

```go
// Acceptable: the argument is cheap to construct.
v, loaded := c.LoadOrStore(key, &counter{})
```

`LoadOrStore` evaluates its argument on **every** call, including the ones that end up
loading an existing entry. That makes it correct only when constructing the value is
trivial — a zero value, a small struct, an empty slice, a mutex or counter holder.

Never wrap a query, an HTTP call, or any real computation in it: the work runs on every
caller and only one result is kept, which is the stampede again with the cost hidden
inside an argument. Reach for singleflight instead.

## Bound the size

A cache with no eviction policy is a memory leak with a good reputation. Either cap entries with an eviction policy, or key it on a set that is bounded by construction and say so in a comment.

## What not to cache

Anything whose staleness changes what the caller does. If a value must be current, read it. A cache hit that returns a stale value is worse than the query it saved.