package adminauth

import (
	"context"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Class names a rate-limit bucket family (spec route table: ip, user, login).
type Class string

const (
	ClassIP    Class = "ip"
	ClassUser  Class = "user"
	ClassLogin Class = "login"
)

// Rate is one token bucket: R tokens per second, Burst at most. Burst 0 disables the class.
type Rate struct {
	R     rate.Limit
	Burst int
}

// Limits configures a Limiter. The zero value disables every class (tests that do not exercise
// limits); the app always passes DefaultLimits.
type Limits struct {
	IP, User, Login Rate
	MaxEntries      int           // cap on live buckets across classes (D-041: bounded maps)
	Idle            time.Duration // a bucket unused this long is swept
}

// DefaultLimits are the S3 values: per IP 20 rps burst 40, per user 10 rps burst 20, sign-in 5
// per minute burst 5 per IP, 10 000 buckets, 10-minute idle sweep.
func DefaultLimits() Limits {
	return Limits{
		IP:         Rate{R: 20, Burst: 40},
		User:       Rate{R: 10, Burst: 20},
		Login:      Rate{R: rate.Every(time.Minute / 5), Burst: 5},
		MaxEntries: 10_000,
		Idle:       10 * time.Minute,
	}
}

// Limiter is a map of token buckets keyed by class and IP or user id (golang.org/x/time/rate,
// after code-heim go_82_rate_limiting_revised, plus the cap and the sweep it lacks).
//
// Concurrency: mu guards buckets and every entry's seen; it is held only for map work and a
// non-blocking ReserveN (rate.Limiter takes its own mutex inside: lock order mu → rate.Limiter).
// Nothing blocks while mu is held. The sweep goroutine is Run's caller's (the app errgroup).
type Limiter struct {
	lim Limits
	now func() time.Time

	mu        sync.Mutex
	buckets   map[string]*bucket
	lastSweep time.Time // guarded by mu: a full map sweeps at most once a second, not per new key
}

type bucket struct {
	l    *rate.Limiter
	seen time.Time // guarded by Limiter.mu
}

// NewLimiter builds a Limiter; now is the clock (nil = time.Now).
func NewLimiter(l Limits, now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	return &Limiter{lim: l, now: now, buckets: map[string]*bucket{}}
}

func (l *Limiter) rateOf(c Class) Rate {
	switch c {
	case ClassIP:
		return l.lim.IP
	case ClassUser:
		return l.lim.User
	case ClassLogin:
		return l.lim.Login
	}
	return Rate{}
}

// Allow takes one token from the (class, key) bucket. When refused, retry is how long until a
// token would be available (the Retry-After value). A new key while the map is at MaxEntries is
// refused after a sweep frees nothing: fail closed, existing buckets keep working.
func (l *Limiter) Allow(c Class, key string) (ok bool, retry time.Duration) {
	r := l.rateOf(c)
	if r.Burst <= 0 {
		return true, 0
	}
	now := l.now()
	k := string(c) + "|" + key
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.buckets[k]
	if b == nil {
		if l.lim.MaxEntries > 0 && len(l.buckets) >= l.lim.MaxEntries {
			if now.Sub(l.lastSweep) >= time.Second {
				l.sweepLocked(now)
			}
			if len(l.buckets) >= l.lim.MaxEntries {
				return false, time.Second
			}
		}
		b = &bucket{l: rate.NewLimiter(r.R, r.Burst)}
		l.buckets[k] = b
	}
	b.seen = now
	res := b.l.ReserveN(now, 1)
	if !res.OK() {
		return false, time.Second
	}
	if d := res.DelayFrom(now); d > 0 {
		res.CancelAt(now) // a refused request must not spend the token it would have waited for
		return false, d
	}
	return true, 0
}

// Len is the number of live buckets (tests, leak checks).
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// Sweep drops every bucket idle for Limits.Idle or longer and returns how many it dropped.
func (l *Limiter) Sweep() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.sweepLocked(l.now())
}

func (l *Limiter) sweepLocked(now time.Time) int {
	l.lastSweep = now
	idle := l.lim.Idle
	if idle <= 0 {
		return 0
	}
	n := 0
	for k, b := range l.buckets {
		if now.Sub(b.seen) >= idle {
			delete(l.buckets, k)
			n++
		}
	}
	return n
}

// Run sweeps every Limits.Idle until ctx ends, then returns nil. With Idle <= 0 it only waits
// for ctx (there is nothing to sweep on a schedule).
func (l *Limiter) Run(ctx context.Context) error {
	if l.lim.Idle <= 0 {
		<-ctx.Done()
		return nil
	}
	t := time.NewTicker(l.lim.Idle)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			l.Sweep()
		}
	}
}
