package sheet

import (
	"strings"
	"time"
)

// sweepEntry is one logged write: the seq it applied at, its kind and its key, prefix or suffix.
type sweepEntry struct {
	seq  uint64
	kind OpKind
	arg  string
	at   time.Time
}

// covers reports whether this logged write touched key.
func (e sweepEntry) covers(key string) bool {
	switch e.kind {
	case OpSet, OpDelete:
		return e.arg == key
	case OpDeletePrefix:
		return strings.HasPrefix(key, e.arg)
	case OpDeleteSuffix:
		return strings.HasSuffix(key, e.arg)
	}
	return false
}

// sweepLog is the per-sheet bounded log of sets and deletes behind R6: a set whose base_seq is older
// than a logged write covering its key is refused, so a read-through value fetched before a sweep
// cannot be re-cached after it. Entries are pruned past window (by age) and max (by count); floor is
// the highest pruned seq, and any base_seq below it is refused because the log can no longer prove
// the write is fresh. Guarded by the sheet mutex.
//
// New (no advo_cache counterpart): replaces backendAdvo's double sweep 2 s apart
// (backendAdvo/cache/store.go:36-39, G-01-16).
type sweepLog struct {
	entries []sweepEntry // ascending seq
	floor   uint64
	window  time.Duration // <= 0: no age bound
	max     int           // <= 0: no count bound
}

// prune drops entries older than window and, when room is needed for one more, beyond max. A prune
// that empties the log sets entries to nil, so an idle sheet does not keep the backing array.
func (l *sweepLog) prune(now time.Time, adding bool) {
	drop := 0
	if l.window > 0 {
		cutoff := now.Add(-l.window)
		for drop < len(l.entries) && l.entries[drop].at.Before(cutoff) {
			drop++
		}
	}
	if l.max > 0 {
		limit := l.max
		if adding {
			limit--
		}
		if over := len(l.entries) - drop - limit; over > 0 {
			drop += over
		}
	}
	if drop == 0 {
		return
	}
	l.floor = max(l.floor, l.entries[drop-1].seq)
	if drop == len(l.entries) {
		l.entries = nil
		return
	}
	l.entries = l.entries[drop:]
}

// add records a write applied at seq.
func (l *sweepLog) add(seq uint64, kind OpKind, arg string, now time.Time) {
	l.prune(now, true)
	l.entries = append(l.entries, sweepEntry{seq: seq, kind: kind, arg: arg, at: now})
}

// stale reports whether a set of key based on base must be refused.
func (l *sweepLog) stale(base uint64, key string, now time.Time) bool {
	l.prune(now, false)
	if base < l.floor {
		return true
	}
	for i := len(l.entries) - 1; i >= 0 && l.entries[i].seq > base; i-- {
		if l.entries[i].covers(key) {
			return true
		}
	}
	return false
}

// reset forgets every entry and sets the floor (Replace: the log cannot vouch for anything older).
func (l *sweepLog) reset(floor uint64) {
	l.entries = nil
	l.floor = floor
}
