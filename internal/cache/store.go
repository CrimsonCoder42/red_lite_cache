package cache

import (
	"strings"
	"sync"
	"time"
)

// keySet is one bucket of the segment index.
type keySet map[string]struct{}

// Store is one sheet's key/value map. Get and Set are O(1). DeletePrefix and DeleteSuffix visit
// only the keys that share the pattern's first (prefix) or last (suffix) ':'-separated segment, so
// a sweep like `<root>:patient:<uuid>:` or `:permissions` never scans the whole sheet (R7).
//
// Bounded by the sheet's MaxKeys (enforced by internal/sheet) and by TTL expiry; the store itself
// does not evict.
//
// after advo_cache/cache/cache.go:18-22: a sync.Map per sheet became map + RWMutex (D-002), the
// value carries its own expiry instead of a goroutine per Set (K5, cache.go:44-51), and two segment
// indexes replace the full Range in DeletePrefix/DeleteSuffix (cache.go:62-88).
type Store struct {
	mu    sync.RWMutex
	m     map[string]Entry
	roots map[string]keySet // first segment (text before the first ':', or the whole key) → keys
	tails map[string]keySet // last segment (text after the last ':', or the whole key) → keys
}

// New returns an empty store.
func New() *Store {
	return &Store{m: make(map[string]Entry), roots: make(map[string]keySet), tails: make(map[string]keySet)}
}

// rootOf is the text before the first ':' (the whole string when there is none).
func rootOf(key string) string {
	if i := strings.IndexByte(key, ':'); i >= 0 {
		return key[:i]
	}
	return key
}

// tailOf is the text after the last ':' (the whole string when there is none).
func tailOf(key string) string {
	if i := strings.LastIndexByte(key, ':'); i >= 0 {
		return key[i+1:]
	}
	return key
}

// index adds a new key to both segment indexes. Caller holds mu for writing.
func (s *Store) index(key string) {
	r := rootOf(key)
	if s.roots[r] == nil {
		s.roots[r] = make(keySet)
	}
	s.roots[r][key] = struct{}{}
	t := tailOf(key)
	if s.tails[t] == nil {
		s.tails[t] = make(keySet)
	}
	s.tails[t][key] = struct{}{}
}

// remove deletes key from the map and both indexes, dropping an index bucket when it empties.
// Caller holds mu for writing and knows key is present.
func (s *Store) remove(key string) {
	delete(s.m, key)
	r := rootOf(key)
	if set := s.roots[r]; set != nil {
		delete(set, key)
		if len(set) == 0 {
			delete(s.roots, r)
		}
	}
	t := tailOf(key)
	if set := s.tails[t]; set != nil {
		delete(set, key)
		if len(set) == 0 {
			delete(s.tails, t)
		}
	}
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
	_, had := s.m[key]
	s.m[key] = e
	if !had {
		s.index(key)
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
// A prefix containing ':' visits only the keys of its first segment; a prefix without ':' ends
// inside the first segment, so it visits the index's root names and takes every key under each
// matching root.
func (s *Store) DeletePrefix(prefix string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if prefix == "" {
		return s.clear()
	}
	var victims []string
	if i := strings.IndexByte(prefix, ':'); i >= 0 {
		for k := range s.roots[prefix[:i]] {
			if strings.HasPrefix(k, prefix) {
				victims = append(victims, k)
			}
		}
	} else {
		for root, set := range s.roots {
			if strings.HasPrefix(root, prefix) {
				for k := range set {
					victims = append(victims, k)
				}
			}
		}
	}
	for _, k := range victims {
		s.remove(k)
	}
	return len(victims)
}

// DeleteSuffix removes every key ending with suffix and returns how many. "" removes every key
// (every string ends with ""). A suffix containing ':' visits only the keys of its last segment; a
// suffix without ':' visits the index's tail names and takes every key under each matching tail.
func (s *Store) DeleteSuffix(suffix string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if suffix == "" {
		return s.clear()
	}
	var victims []string
	if i := strings.LastIndexByte(suffix, ':'); i >= 0 {
		for k := range s.tails[suffix[i+1:]] {
			if strings.HasSuffix(k, suffix) {
				victims = append(victims, k)
			}
		}
	} else {
		for tail, set := range s.tails {
			if strings.HasSuffix(tail, suffix) {
				for k := range set {
					victims = append(victims, k)
				}
			}
		}
	}
	for _, k := range victims {
		s.remove(k)
	}
	return len(victims)
}

// clear empties the store and returns how many keys it held. Caller holds mu for writing.
func (s *Store) clear() int {
	n := len(s.m)
	s.m = make(map[string]Entry)
	s.roots = make(map[string]keySet)
	s.tails = make(map[string]keySet)
	return n
}

// Expire removes every entry expired at now and returns their keys (nil when none). It is the one
// full scan in the store; the sheet janitor calls it once per interval (D-003).
func (s *Store) Expire(now time.Time) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var keys []string
	for k, e := range s.m {
		if e.Expired(now) {
			keys = append(keys, k)
		}
	}
	for _, k := range keys {
		s.remove(k)
	}
	return keys
}

// Len is the number of stored entries, including expired ones Expire has not removed yet.
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.m)
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

// Replace swaps the whole content for entries (replica snapshot, hydration) and rebuilds the
// indexes. The map passed in is copied.
func (s *Store) Replace(entries map[string]Entry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clear()
	for k, e := range entries {
		s.m[k] = e
		s.index(k)
	}
}
