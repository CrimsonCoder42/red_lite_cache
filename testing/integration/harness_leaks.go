package integration

// Leak-suite helpers (D-038, D-039): a settable clock for the server, a goroutine count that
// settles, and HeapAlloc after GC. Used by testing/leaks; every baseline is taken inside the test
// after the harness is up, never at package init.

import (
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/CrimsonCoder42/red_lite_cache/internal/config"
)

// Clock is a manually advanced time source (D-039). The zero value is not usable; use NewClock.
type Clock struct {
	mu  sync.Mutex
	now time.Time
}

// NewClock returns a clock at the current wall time, so expiries computed from it look normal.
func NewClock() *Clock { return &Clock{now: time.Now()} }

// Now is the clock's current time.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the clock forward by d.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// WithClock is a StartLeader/StartReplica mod: the instance's sheet registry (store expiry,
// janitor, sweep log) and lock manager read c instead of time.Now. The janitor and lock cleaner
// still tick on real time (TestConfig: 50 ms); what they compare against is c.
func WithClock(c *Clock) func(*config.Config) {
	return func(cfg *config.Config) { cfg.Clock = c.Now }
}

// Settle parameters (D-038): sample every settleTick, up to settleSamples times, until two
// consecutive samples agree.
const (
	settleSamples = 20
	settleTick    = 10 * time.Millisecond
)

// Goroutines returns runtime.NumGoroutine once it is stable: two consecutive samples
// settleTick apart that agree, within settleSamples samples. If it never settles, the last sample is
// returned and the test logs it.
func Goroutines(t testing.TB) int {
	t.Helper()
	prev := runtime.NumGoroutine()
	for range settleSamples {
		time.Sleep(settleTick)
		cur := runtime.NumGoroutine()
		if cur == prev {
			return cur
		}
		prev = cur
	}
	t.Logf("goroutines did not settle in %d samples; last %d", settleSamples, prev)
	return prev
}

// GoroutineBaseline is the D-038 BASELINE for goroutines: two settled passes, the second returned.
// Call it after the harness (leader, replica, readers) is up.
func GoroutineBaseline(t testing.TB) int {
	t.Helper()
	Goroutines(t)
	return Goroutines(t)
}

// WaitGoroutines waits (bounded by Timeout) until the settled goroutine count is at most
// want+tolerance, and returns the last settled count. A goroutine still exiting after a close is
// what this waits for; the caller asserts on the result.
func WaitGoroutines(t testing.TB, want, tolerance int) int {
	t.Helper()
	deadline := time.Now().Add(Timeout)
	n := Goroutines(t)
	for n > want+tolerance && time.Now().Before(deadline) {
		time.Sleep(settleTick)
		n = Goroutines(t)
	}
	return n
}

// HeapAlloc is runtime.MemStats.HeapAlloc after two runtime.GC calls (D-038: never Sys).
func HeapAlloc() uint64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}
