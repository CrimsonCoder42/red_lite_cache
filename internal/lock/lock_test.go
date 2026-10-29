// Covers: internal/lock
package lock

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type clock struct{ t atomic.Int64 }

func (c *clock) now() time.Time          { return time.Unix(0, c.t.Load()) }
func (c *clock) advance(d time.Duration) { c.t.Add(int64(d)) }
func newClock() *clock                   { c := &clock{}; c.t.Store(time.Unix(1000, 0).UnixNano()); return c }
func mustErr(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

func TestAcquireHeldRefreshRelease(t *testing.T) {
	c := newClock()
	m := NewManager(c.now)
	if _, err := m.TryAcquire("s", "k", 1, time.Second); err != nil {
		t.Fatal(err)
	}
	_, err := m.TryAcquire("s", "k", 2, time.Second)
	mustErr(t, err, ErrLockHeld)
	// same key in another sheet is independent
	if _, err := m.TryAcquire("s2", "k", 2, time.Second); err != nil {
		t.Fatal(err)
	}
	// same owner refreshes
	c.advance(900 * time.Millisecond)
	exp, err := m.TryAcquire("s", "k", 1, time.Second)
	if err != nil || !exp.Equal(c.now().Add(time.Second)) {
		t.Fatalf("refresh: exp %v err %v", exp, err)
	}
	c.advance(900 * time.Millisecond) // past the first TTL, inside the refreshed one
	_, err = m.TryAcquire("s", "k", 2, time.Second)
	mustErr(t, err, ErrLockHeld)
	mustErr(t, m.Release("s", "k", 2), ErrNotOwner)
	if err := m.Release("s", "k", 1); err != nil {
		t.Fatal(err)
	}
	mustErr(t, m.Release("s", "k", 1), ErrLockNotFound)
}

func TestExpiredTakeoverAndRelease(t *testing.T) {
	c := newClock()
	m := NewManager(c.now)
	m.TryAcquire("s", "k", 1, time.Second)
	c.advance(time.Second)
	if _, err := m.TryAcquire("s", "k", 2, time.Second); err != nil {
		t.Fatalf("expired lock not takeable: %v", err)
	}
	mustErr(t, m.Release("s", "k", 1), ErrNotOwner)
	if n := m.ReleaseAll(1); n != 0 {
		t.Fatalf("owner 1 still indexed: %d", n)
	}
	// an expired lock held by you is gone: ErrLockNotFound, and it is removed
	c.advance(time.Second)
	mustErr(t, m.Release("s", "k", 2), ErrLockNotFound)
	if m.Len() != 0 {
		t.Fatalf("Len = %d", m.Len())
	}
}

func TestReleaseAllAndCleanup(t *testing.T) {
	c := newClock()
	m := NewManager(c.now)
	m.TryAcquire("s", "a", 1, time.Second)
	m.TryAcquire("s", "b", 1, 3*time.Second)
	m.TryAcquire("s", "c", 2, time.Second)
	if n := m.ReleaseAll(1); n != 2 {
		t.Fatalf("ReleaseAll = %d", n)
	}
	if n := m.ReleaseAll(1); n != 0 {
		t.Fatalf("second ReleaseAll = %d", n)
	}
	c.advance(2 * time.Second)
	if n := m.CleanupExpired(c.now()); n != 1 || m.Len() != 0 {
		t.Fatalf("cleanup = %d len %d", n, m.Len())
	}
	if n := m.ReleaseAll(2); n != 0 {
		t.Fatalf("owner index kept a cleaned lock: %d", n)
	}
}

func TestConcurrentAcquireOneWinner(t *testing.T) {
	m := NewManager(nil)
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := range 64 {
		wg.Go(func() {
			if _, err := m.TryAcquire("s", "k", uint64(i+1), time.Minute); err == nil {
				wins.Add(1)
			}
		})
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("winners = %d", wins.Load())
	}
}
