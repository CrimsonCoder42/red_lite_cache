package cache

import (
	"math/rand/v2"
	"time"
)

// Policy is the server's TTL policy for a set: the default lifetime and the jitter added to every
// lifetime.
type Policy struct {
	DefaultTTL    time.Duration // the lifetime of a set with ttl 0/absent; 0 = no expiry
	JitterPercent int           // 0..JitterPercent% of the lifetime is added to every expiry
}

// ExpiresAt is when a value set at now with ttlMinutes expires. ttlMinutes <= 0 uses DefaultTTL; a
// resulting lifetime of 0 is the zero time (no expiry). Every non-zero lifetime gets a uniform
// 0..JitterPercent% added.
//
// after advo_cache/cache/cache.go:147-153 (SetWithJitter): same 0..jitterPercent% uniform spread, but
// applied to every TTL as server policy instead of a per-request flag, and returned as an absolute
// expiry stored in the Entry instead of a sleep duration (K5); math/rand/v2 instead of math/rand.
func (p Policy) ExpiresAt(now time.Time, ttlMinutes int) time.Time {
	ttl := p.DefaultTTL
	if ttlMinutes > 0 {
		ttl = time.Duration(ttlMinutes) * time.Minute
	}
	if ttl <= 0 {
		return time.Time{}
	}
	if p.JitterPercent > 0 {
		jitterMax := int64(ttl) * int64(p.JitterPercent) / 100
		ttl += time.Duration(rand.Int64N(jitterMax + 1))
	}
	return now.Add(ttl)
}
