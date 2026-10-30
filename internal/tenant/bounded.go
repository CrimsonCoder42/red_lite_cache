package tenant

import "github.com/CrimsonCoder42/red_lite_cache/internal/logger"

// DefaultMemoryMax is the cap on each of the revoked-name and dead-key sets when SetMemoryMax was
// not called or was given n ≤ 0 (REDLITE_TENANT_MEMORY_MAX default, D-041).
const DefaultMemoryMax = 10_000

// fifoSet is a set that keeps at most cap members and drops the oldest-inserted first (D-041).
// A map answers has in O(1); order is the insertion queue (order[head:] are the live members, in
// insertion order), compacted once the consumed prefix outgrows the live part, so add is O(1)
// amortised. Not safe for concurrent use: the Directory's mutex guards it.
//
// Why dropping the oldest is safe:
//   - dead key ids: the store is the record of a revoke. MemStore.RevokeKey/RotateKey/RevokeProject
//     (internal/api/store.go, revoke.go) set RevokedAt under the store lock and MemStore.ResolveKey
//     answers only live keys (m.live); DBStore.ResolveKey reads GetActiveAPIKeyByHash (unrevoked
//     rows only). Both commit before the API calls MarkKeyRevoked (tenancy.go revokeKey/rotateKey →
//     closeKeySessions). So a revoked key is refused by Admit through ErrUnknownKey whether or not its
//     id is still here; deadKey only covers the open-vs-revoke window — an Admit that resolved the key
//     just before the revoke committed and is between Admit and the router's KeyRevoked check
//     (internal/ws/router.go open). That window is one open's duration; an id is dropped only after
//     cap newer revokes, which on any real node is far longer than that window.
//   - revoked sheet names: only password-mode autocreate reads them (internal/ws/admit.go), and
//     autocreate is development only (internal/config refuses it elsewhere). A dropped name can be
//     autocreated again in development after cap newer revoked names.
type fifoSet[K comparable] struct {
	name    string // the set's name in the Warn line
	cap     int
	m       map[K]struct{}
	order   []K
	head    int
	dropped uint64 // members dropped by the bound since boot
	warned  bool   // the first drop was logged (once per set per process)
}

func newFIFOSet[K comparable](name string, cap int) *fifoSet[K] {
	return &fifoSet[K]{name: name, cap: cap, m: map[K]struct{}{}}
}

func (s *fifoSet[K]) len() int { return len(s.m) }

func (s *fifoSet[K]) has(k K) bool {
	_, ok := s.m[k]
	return ok
}

// add inserts k (a member already present keeps its place) and drops the oldest members beyond cap.
func (s *fifoSet[K]) add(k K) {
	if _, ok := s.m[k]; ok {
		return
	}
	s.m[k] = struct{}{}
	s.order = append(s.order, k)
	s.trim()
}

// setCap changes the cap and drops the oldest members beyond it.
func (s *fifoSet[K]) setCap(n int) {
	s.cap = n
	s.trim()
}

// reset empties the set; the drop counter and the warned flag are kept.
func (s *fifoSet[K]) reset() {
	clear(s.m)
	s.order, s.head = nil, 0
}

func (s *fifoSet[K]) trim() {
	n := 0
	for len(s.m) > s.cap {
		var zero K
		delete(s.m, s.order[s.head])
		s.order[s.head] = zero // let the dropped member be collected
		s.head++
		n++
	}
	if s.head > 0 && s.head >= len(s.order)-s.head { // consumed prefix ≥ live part: compact
		s.order = append(make([]K, 0, max(len(s.order)-s.head, 16)), s.order[s.head:]...)
		s.head = 0
	}
	if n == 0 {
		return
	}
	s.dropped += uint64(n)
	if !s.warned {
		s.warned = true
		logger.L.Warn("API: tenancy memory bound reached, oldest dropped", "set", s.name, "cap", s.cap, "dropped", n)
	}
}
