package cache

import (
	"strings"
	"sync"
	"time"
)

// keySet is one bucket of the segment index.
type keySet map[string]struct{}

// patientSeg is the second segment that marks a profile-scoped key `<root>:patient:<uuid>[:…]`.
const patientSeg = "patient"

// Store is one sheet's key/value map. Get and Set are O(1). DeletePrefix and DeleteSuffix visit
// only the keys that share the pattern's first (prefix) or last (suffix) ':'-separated segment, so
// a sweep like `<root>:patient:<uuid>:` or `:permissions` never scans the whole sheet (R7). A key
// shaped `<root>:patient:<uuid>[:…]` is also indexed by its head `<root>:patient:<uuid>` (D-037),
// so the consumer's profile sweep (domainPatientPrefix, no trailing ':') visits that root's head
// names and the matching heads' keys instead of every key of the root.
//
// Bounded by the sheet's MaxKeys (enforced by internal/sheet) and by TTL expiry; the store itself
// does not evict.
//
// after advo_cache/cache/cache.go:18-22: a sync.Map per sheet became map + RWMutex (D-002), the
// value carries its own expiry instead of a goroutine per Set (K5, cache.go:44-51), and segment
// indexes replace the full Range in DeletePrefix/DeleteSuffix (cache.go:62-88); D-037 adds the
// patient-head index.
type Store struct {
	mu    sync.RWMutex
	m     map[string]Entry
	roots map[string]keySet // first segment (text before the first ':', or the whole key) → keys
	tails map[string]keySet // last segment (text after the last ':', or the whole key) → keys
	// heads: root → head `<root>:patient:<uuid>` → keys under that head. nil when the store was
	// built by NewWithoutHeadIndex.
	heads map[string]map[string]keySet
}

// New returns an empty store with every index.
func New() *Store {
	s := &Store{}
	s.reset(true)
	return s
}

// NewWithoutHeadIndex returns an empty store with only the first/last-segment index. It exists for
// the D-037 benchmark (testing/conformance B1), which compares the two; production uses New.
func NewWithoutHeadIndex() *Store {
	s := &Store{}
	s.reset(false)
	return s
}

// reset empties the map and every index; withHeads turns the head index on. Caller holds mu for
// writing or owns s exclusively.
func (s *Store) reset(withHeads bool) {
	s.m = make(map[string]Entry)
	s.roots = make(map[string]keySet)
	s.tails = make(map[string]keySet)
	s.heads = nil
	if withHeads {
		s.heads = make(map[string]map[string]keySet)
	}
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

// headOf splits a `<root>:patient:<uuid>[:…]` key or prefix into its root and its head
// `<root>:patient:<uuid>`. closed reports that ':' follows the uuid (the uuid segment is complete).
// ok is false for any other shape and for an empty uuid.
func headOf(key string) (root, head string, closed, ok bool) {
	i := strings.IndexByte(key, ':')
	if i < 0 || !strings.HasPrefix(key[i+1:], patientSeg+":") {
		return "", "", false, false
	}
	start := i + 1 + len(patientSeg) + 1
	end := len(key)
	if j := strings.IndexByte(key[start:], ':'); j >= 0 {
		end, closed = start+j, true
	}
	if end == start {
		return "", "", false, false
	}
	return key[:i], key[:end], closed, true
}

// index adds a new key to every index. Caller holds mu for writing.
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
	if s.heads == nil {
		return
	}
	if root, head, _, ok := headOf(key); ok {
		hs := s.heads[root]
		if hs == nil {
			hs = make(map[string]keySet)
			s.heads[root] = hs
		}
		if hs[head] == nil {
			hs[head] = make(keySet)
		}
		hs[head][key] = struct{}{}
	}
}

// remove deletes key from the map and every index, dropping an index bucket when it empties. When
// the last key goes, the maps are replaced by fresh ones: a Go map never shrinks on delete, so a
// store drained key by key (expiry, sweeps) would otherwise keep its peak bucket arrays for life.
// Caller holds mu for writing and knows key is present.
func (s *Store) remove(key string) {
	delete(s.m, key)
	if len(s.m) == 0 {
		s.reset(s.heads != nil)
		return
	}
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
	if s.heads == nil {
		return
	}
	root, head, _, ok := headOf(key)
	if !ok {
		return
	}
	if hs := s.heads[root]; hs != nil {
		if set := hs[head]; set != nil {
			delete(set, key)
			if len(set) == 0 {
				delete(hs, head)
			}
		}
		if len(hs) == 0 {
			delete(s.heads, root)
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
//
//   - `<root>:patient:<uuid>:…` (the uuid segment closed by ':') visits only that head's keys.
//   - `<root>:patient:<uuid>` (ending inside or at the end of the uuid segment) visits that root's
//     head names and takes every key under each head the prefix starts; every key it can match has
//     a head, so nothing else is visited.
//   - any other prefix containing ':' visits only the keys of its first segment.
//   - a prefix without ':' ends inside the first segment: it visits the root names and takes every
//     key under each matching root.
func (s *Store) DeletePrefix(prefix string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if prefix == "" {
		return s.clear()
	}
	var victims []string
	root, head, closed, isHead := headOf(prefix)
	switch {
	case isHead && s.heads != nil && closed:
		for k := range s.heads[root][head] {
			if strings.HasPrefix(k, prefix) {
				victims = append(victims, k)
			}
		}
	case isHead && s.heads != nil:
		for h, set := range s.heads[root] {
			if strings.HasPrefix(h, prefix) {
				for k := range set {
					victims = append(victims, k)
				}
			}
		}
	case strings.IndexByte(prefix, ':') >= 0:
		for k := range s.roots[rootOf(prefix)] {
			if strings.HasPrefix(k, prefix) {
				victims = append(victims, k)
			}
		}
	default:
		for r, set := range s.roots {
			if strings.HasPrefix(r, prefix) {
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
	s.reset(s.heads != nil)
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

// Stats is a store's size: counts only, never keys or values (D-040). Diagnostic; the leak suite
// (testing/leaks) asserts every field returns to 0 once the store is drained.
type Stats struct {
	Entries     int // stored entries, expired-but-unswept included
	RootBuckets int // first-segment index buckets
	TailBuckets int // last-segment index buckets
	HeadBuckets int // patient-head buckets summed over every root (0 without the head index)
	HeadRoots   int // roots holding at least one head bucket
}

// Stats counts the map and every index bucket under the read lock.
func (s *Store) Stats() Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st := Stats{Entries: len(s.m), RootBuckets: len(s.roots), TailBuckets: len(s.tails), HeadRoots: len(s.heads)}
	for _, hs := range s.heads {
		st.HeadBuckets += len(hs)
	}
	return st
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
