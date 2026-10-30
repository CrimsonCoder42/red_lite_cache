package lock

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/CrimsonCoder42/red_lite_cache/internal/logger"
)

// Sentinel errors. internal/ws maps each to its protocol.ErrMsg* string.
var (
	ErrLockHeld     = errors.New("lock: held by another owner")
	ErrNotOwner     = errors.New("lock: not owner")
	ErrLockNotFound = errors.New("lock: not found")
)

// lockKey scopes a lock to one sheet.
type lockKey struct{ sheet, key string }

type held struct {
	owner     uint64
	expiresAt time.Time
}

// Manager holds every lock of the process. A lock belongs to one connection (owner = ws.Conn.ID()),
// expires after its TTL, and is released when its owner disconnects. Locks are not replicated: a
// failover drops the connections that own them (doc/ASSUMPTIONS.md).
//
// after advo_cache/lock/lock.go:34-153: sync.Map keyed "sheet:key" became map + Mutex keyed by a
// struct (no separator collision), owner is the connection's uint64 id instead of a %p string, and
// an owner index makes ReleaseAll O(locks owned) instead of a Range over every lock.
type Manager struct {
	now func() time.Time

	mu      sync.Mutex
	locks   map[lockKey]held
	byOwner map[uint64]map[lockKey]struct{}
}

// NewManager returns an empty manager. now nil = time.Now.
func NewManager(now func() time.Time) *Manager {
	if now == nil {
		now = time.Now
	}
	return &Manager{now: now, locks: make(map[lockKey]held), byOwner: make(map[uint64]map[lockKey]struct{})}
}

// TryAcquire takes the lock on (sheet, key) for owner for ttl, without blocking. The same owner
// re-locking refreshes the TTL; an expired lock is taken over. It returns the new expiry, or
// ErrLockHeld when another owner holds a live lock.
//
// after advo_cache/lock/lock.go:53-96: the Load / LoadOrStore / Store sequence (which could let two
// callers both "win") is one critical section.
func (m *Manager) TryAcquire(sheet, key string, owner uint64, ttl time.Duration) (time.Time, error) {
	k := lockKey{sheet, key}
	now := m.now()
	exp := now.Add(ttl)
	m.mu.Lock()
	defer m.mu.Unlock()
	if h, ok := m.locks[k]; ok && h.owner != owner && now.Before(h.expiresAt) {
		return time.Time{}, fmt.Errorf("lock: acquire %q in %q: %w", key, sheet, ErrLockHeld)
	} else if ok && h.owner != owner {
		m.dropLocked(k, h.owner) // expired: take it over
	}
	m.locks[k] = held{owner: owner, expiresAt: exp}
	idx := m.byOwner[owner]
	if idx == nil {
		idx = make(map[lockKey]struct{})
		m.byOwner[owner] = idx
	}
	idx[k] = struct{}{}
	return exp, nil
}

// Release frees the lock on (sheet, key) held by owner. ErrLockNotFound when there is no live lock
// — an expired lock counts as gone, whoever held it, and is removed; ErrNotOwner when another owner
// holds it.
//
// after advo_cache/lock/lock.go:98-114: an expired lock is "not found" instead of releasable by its
// old owner or "not owner" for everyone else.
func (m *Manager) Release(sheet, key string, owner uint64) error {
	k := lockKey{sheet, key}
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	h, ok := m.locks[k]
	if !ok {
		return fmt.Errorf("lock: release %q in %q: %w", key, sheet, ErrLockNotFound)
	}
	if !now.Before(h.expiresAt) {
		m.dropLocked(k, h.owner)
		return fmt.Errorf("lock: release %q in %q: %w", key, sheet, ErrLockNotFound)
	}
	if h.owner != owner {
		return fmt.Errorf("lock: release %q in %q: %w", key, sheet, ErrNotOwner)
	}
	m.dropLocked(k, owner)
	return nil
}

// ReleaseAll frees every lock owner holds (disconnect) and returns how many. O(locks owned).
//
// after advo_cache/lock/lock.go:133-153 (ReleaseAllByOwner): the owner index replaces the Range.
func (m *Manager) ReleaseAll(owner uint64) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	idx := m.byOwner[owner]
	for k := range idx {
		delete(m.locks, k)
	}
	delete(m.byOwner, owner)
	return len(idx)
}

// CleanupExpired removes every lock expired at now and returns how many.
//
// after advo_cache/lock/lock.go:155-175: one pass under one lock, no second key slice.
func (m *Manager) CleanupExpired(now time.Time) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for k, h := range m.locks {
		if !now.Before(h.expiresAt) {
			m.dropLocked(k, h.owner)
			n++
		}
	}
	return n
}

// Len is the number of locks held, expired ones included until cleanup.
func (m *Manager) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.locks)
}

// Stats is a point-in-time count of the manager's two maps (D-038 leak suite). Counts only: no
// sheet, key or owner id leaves the package through it.
type Stats struct {
	Held   int // len(locks), expired ones included until cleanup
	Owners int // len(byOwner): owners holding at least one lock
}

// Stats returns the current counts under one lock.
func (m *Manager) Stats() Stats {
	m.mu.Lock()
	defer m.mu.Unlock()
	return Stats{Held: len(m.locks), Owners: len(m.byOwner)}
}

// RunCleaner runs CleanupExpired every interval until ctx is done, then returns nil. The caller owns
// the goroutine (app's errgroup).
//
// after advo_cache/main.go:50-55: a bare `go func` over a never-stopped ticker became a ctx-cancelled
// loop (code-heim go_18_worker_pool shape, doc/reference/codeheim_patterns.md § Tier 2).
func (m *Manager) RunCleaner(ctx context.Context, interval time.Duration) error {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if n := m.CleanupExpired(m.now()); n > 0 {
				logger.L.Debug("Lock: expired", "locks", n)
			}
		}
	}
}

// dropLocked removes k (held by owner) from both maps. Caller holds mu.
func (m *Manager) dropLocked(k lockKey, owner uint64) {
	delete(m.locks, k)
	if idx := m.byOwner[owner]; idx != nil {
		delete(idx, k)
		if len(idx) == 0 {
			delete(m.byOwner, owner)
		}
	}
}
