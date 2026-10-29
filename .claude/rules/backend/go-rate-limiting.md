---
paths:
  - "internal/ws/**/*.go"
  - "internal/api/**/*.go"
---

# Rate Limiting

_Origin: code-heim go_77_rate_limit and go_82_rate_limiting_revised (`doc/reference/codeheim_patterns.md` § Tier 3). Per-IP, never evicted, and go_82 does not compile; per-identity and janitor-evicted here._

Applies whenever rate limiting is added or changed. `golang.org/x/time/rate` provides the token bucket; the decisions below are what make it correct here.

## Key on identity, not IP, wherever identity exists

- Authenticated routes key on user or tenant ID. A clinic, a family, or anyone behind NAT shares one public IP, and IP-keying throttles them collectively.
- IP-keying is correct only where there is no identity yet: login, signup, password reset, and other pre-auth endpoints. Those are also the endpoints that most need it.

## State goes in the shared cache, never in process memory

- A per-process `map` or `sync.Map` of limiters means the effective limit multiplies by the instance count, and limits reset on every deploy.
- Use `cache *cache.Store`, the same dependency handlers already take.
- If a local map is ever justified for a single-instance tool, it must have TTL eviction. An unbounded limiter map is a memory leak and a DoS vector: spoofed source IPs grow it until the process dies.

## Client IP behind a proxy

- `r.RemoteAddr` behind a load balancer is the balancer's address, which collapses every client into one bucket.
- Read the forwarded header only for the hop we control, and treat it as untrusted otherwise. A blindly trusted `X-Forwarded-For` lets a caller pick their own rate limit key and bypass limiting entirely.

## Response

- Exceeding the limit returns `429` with the standard JSON error shape.
- Include `Retry-After`. A client that does not know when to retry will hammer.
- Never leak the limit, the remaining count, or the window to unauthenticated callers.

## Concurrency

- Any shared limiter structure is accessed concurrently, because every request runs in its own goroutine. A plain `map` without a mutex is a data race and will corrupt under load.
- `sync.Map` removes the explicit lock but does not solve eviction or the multi-instance problem. It is not the fix.

## Do not adopt tutorial implementations directly

Public examples of this pattern almost always use an in-memory map keyed on `RemoteAddr` with no eviction. That shape is fine for a demo and wrong for this service on all three counts above.