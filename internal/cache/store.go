package cache

import (
	"sync"
	"time"

	"github.com/CrimsonCoder42/red_lite_cache/internal/logger"
)

// Store is one sheet's key/value map. Get and Set are O(1). DeletePrefix and DeleteSuffix find
// their keys through Groups (groups.go): one descent in a sorted index plus a walk over the
// matches, for a prefix or suffix of any length and any key shape, so no sweep scans the whole
// sheet (R7, D-044).
//
// Bounded by the sheet's MaxKeys (enforced by internal/sheet) and by TTL expiry; the store itself
// does not evict.
//
// after advo_cache/cache/cache.go:18-22: a sync.Map per sheet became map + RWMutex (D-002), the
// value carries its own expiry instead of a goroutine per Set (K5, cache.go:44-51), and an index
// replaces the full Range in DeletePrefix/DeleteSuffix (cache.go:62-88): first segment indexes
// (D-037), then the ordered Groups shared with the reader (D-044).
type Store struct {
	mu sync.RWMutex
	m  map[string]Entry
	// groups indexes exactly the keys of m; every add and remove of a key goes through Set,
	// remove, DeletePrefix/DeleteSuffix, clear or Replace, which keep the two in step.
	groups *Groups
	// wheel files every entry with an expiry under its second (D-045); Expire walks it instead of
	// the map. expiring counts the entries of m with a non-zero ExpiresAt: the wheel's live items,
	// which bound its stale ones (Compact). Both under mu.
	wheel    *Wheel
	expiring int
}

// New returns an empty store.
func New() *Store {
	s := &Store{}
	s.reset()
	return s
}

// reset empties the map, the index and the wheel. Caller holds mu for writing or owns s
// exclusively.
func (s *Store) reset() {
	s.m = make(map[string]Entry)
	if s.groups == nil {
		s.groups, s.wheel = NewGroups(), NewWheel()
	} else {
		s.groups.Reset()
		s.wheel.Reset()
	}
	s.expiring = 0
}

// expiryOf is the wheel's view of key: its live expiry, false when it is gone. Caller holds mu.
func (s *Store) expiryOf(key string) (time.Time, bool) {
	e, ok := s.m[key]
	return e.ExpiresAt, ok
}

// unmap deletes a present key from the map (not the index) and keeps expiring in step; its wheel
// item goes stale. Caller holds mu for writing.
func (s *Store) unmap(key string) {
	if !s.m[key].ExpiresAt.IsZero() {
		s.expiring--
	}
	delete(s.m, key)
}

// remove deletes key from the map, the index and (as a stale item) the wheel. When the last key
// goes, the map is replaced by a fresh one: a Go map never shrinks on delete, so a store drained
// key by key (expiry, sweeps) would otherwise keep its peak bucket array for life. Caller holds mu
// for writing and knows key is present.
func (s *Store) remove(key string) {
	s.unmap(key)
	if len(s.m) == 0 {
		s.reset()
		return
	}
	s.groups.Remove(key)
}

// Get returns the live entry for key. An expired entry is a miss (lazy expiry, D-003); it stays in
// the map until Expire removes it, so the removal is broadcast by the sheet janitor.
func (s *Store) Get(key string, now time.Time) (Entry, bool) {
	s.mu.RLock()
	e, ok := s.m[key]
	s.mu.RUnlock()
	if !ok || e.Expired(now) {
		return Entry{}, false
	}
	return e, true
}

// Has reports whether key holds a live entry at now.
func (s *Store) Has(key string, now time.Time) bool {
	_, ok := s.Get(key, now)
	return ok
}

// Set stores e under key, replacing any previous entry and its expiry (a re-set extends the life;
// no old timer survives, K5). isNew is true when key was not present (expired-but-unswept counts as
// present).
func (s *Store) Set(key string, e Entry) (isNew bool) {
	s.mu.Lock()
	old, had := s.m[key]
	if had && !old.ExpiresAt.IsZero() {
		s.expiring-- // its wheel item goes stale
	}
	s.m[key] = e
	if !had {
		s.groups.Add(key)
	}
	if !e.ExpiresAt.IsZero() {
		s.expiring++
		s.wheel.Add(key, e.ExpiresAt)
		if s.wheel.Compact(s.expiring, s.expiryOf) {
			logger.L.Debug("Cache: expiry wheel compacted", "items", s.wheel.Stats().Items, "live", s.expiring)
		}
	}
	s.mu.Unlock()
	return !had
}

// Delete removes key and reports whether it was present.
func (s *Store) Delete(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[key]; !ok {
		return false
	}
	s.remove(key)
	return true
}

// DeletePrefix removes every key starting with prefix and returns how many. "" removes every key.
// The index cuts the group out (Groups.CutPrefix); only the matches are visited.
func (s *Store) DeletePrefix(prefix string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if prefix == "" {
		return s.clear()
	}
	return s.dropAll(s.groups.CutPrefix(nil, prefix))
}

// DeleteSuffix removes every key ending with suffix and returns how many. "" removes every key
// (every string ends with ""). The reversed index cuts the group out (Groups.CutSuffix).
func (s *Store) DeleteSuffix(suffix string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if suffix == "" {
		return s.clear()
	}
	return s.dropAll(s.groups.CutSuffix(nil, suffix))
}

// dropAll removes keys the index already cut out from the map and returns how many. Caller holds
// mu for writing.
func (s *Store) dropAll(keys []string) int {
	for _, k := range keys {
		s.unmap(k)
	}
	if len(s.m) == 0 {
		s.reset()
	}
	return len(keys)
}

// clear empties the store and returns how many keys it held. Caller holds mu for writing.
func (s *Store) clear() int {
	n := len(s.m)
	s.reset()
	return n
}

// Expire removes every entry expired at now and returns their keys (nil when none). The sheet
// janitor calls it once per interval (D-003). It walks the wheel (D-045): only the seconds since the
// last pass and the keys filed in them, never the whole map, so a pass with nothing due holds mu
// for microseconds whatever the sheet's size.
func (s *Store) Expire(now time.Time) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var keys []string
	s.wheel.Due(now, s.expiryOf, func(k string) {
		keys = append(keys, k)
		s.unmap(k)
		s.groups.Remove(k)
	})
	if len(s.m) == 0 {
		s.reset()
	}
	return keys
}

// Len is the number of stored entries, including expired ones Expire has not removed yet.
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.m)
}

// Stats is a store's size: counts only, never keys or values (D-040). Diagnostic; the leak suite
// (testing/leaks) asserts every field returns to 0 once the store is drained.
type Stats struct {
	Entries      int // stored entries, expired-but-unswept included
	ForwardNodes int // nodes of the key-order index (Groups)
	ReverseNodes int // nodes of the reversed-key index (Groups)
	WheelItems   int // expiry wheel items, stale ones included (at most 2 × live after a Set)
	WheelSlots   int // expiry wheel seconds holding an item
}

// Stats counts the map, the index nodes and the wheel under the read lock.
func (s *Store) Stats() Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	g, w := s.groups.Stats(), s.wheel.Stats()
	return Stats{Entries: len(s.m), ForwardNodes: g.ForwardNodes, ReverseNodes: g.ReverseNodes,
		WheelItems: w.Items, WheelSlots: w.Slots}
}

// All returns a copy of every live entry at now (snapshot, sheet_state, export). Entry values share
// their bytes with the store; nothing mutates a stored Value in place.
func (s *Store) All(now time.Time) map[string]Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]Entry, len(s.m))
	for k, e := range s.m {
		if !e.Expired(now) {
			out[k] = e
		}
	}
	return out
}

// Replace swaps the whole content for entries (replica snapshot, hydration), rebuilds the index in
// one pass (Groups.Load) and files every expiry in the wheel. The map passed in is copied.
func (s *Store) Replace(entries map[string]Entry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clear()
	keys := make([]string, 0, len(entries))
	for k, e := range entries {
		s.m[k] = e
		keys = append(keys, k)
		if !e.ExpiresAt.IsZero() {
			s.expiring++
			s.wheel.Add(k, e.ExpiresAt)
		}
	}
	s.groups.Load(keys)
}
