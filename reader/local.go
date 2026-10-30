package reader

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/CrimsonCoder42/red_lite_cache/protocol"
)

// evictScan is how many entries a full map looks at for an expired one before it evicts an
// arbitrary entry instead. It keeps a Set on a full map O(1).
const evictScan = 64

// localEntry is one cached value: the JSON bytes exactly as the server sent them, never mutated
// after they are stored, and the local deadline derived from the server's expires_in_ms.
type localEntry struct {
	raw       json.RawMessage
	expiresAt time.Time // zero = no expiry
}

func (e localEntry) live(now time.Time) bool {
	return e.expiresAt.IsZero() || now.Before(e.expiresAt)
}

func (e localEntry) isNull() bool { return bytes.Equal(e.raw, protocol.NullValue) }

// expiry turns a relative lifetime into a local deadline. ms <= 0 = no expiry.
func expiry(now time.Time, ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return now.Add(time.Duration(ms) * time.Millisecond)
}

// local is the reader's copy of the sheet.
//
// after advo_cache/reader/reader.go:107,157: sync.Map of decoded values, no expiry, no bound →
// map + RWMutex of raw bytes with a deadline, bounded by max (D-002, D-008).
type local struct {
	mu      sync.RWMutex
	entries map[string]localEntry
	max     int

	// seq is the applied sheet sequence: the sheet_state's seq, then the max of every broadcast's and
	// every ack's seq (R6).
	seq uint64
	// missSeq records, per key, the misses behind base_seq (R6): for each slice of missWindow/2 in
	// which the key missed, the earliest miss (applied seq + time). An entry is live for missWindow
	// after its miss; a set of the key sends the OLDEST live entry's seq and never consumes it.
	// Bounded by missMax keys; on overflow the keys with no live entry are dropped first, and if
	// still full the map is dropped and fallback (the lowest live seq it held, live until the newest
	// dropped entry expires) is sent for any key without a live entry, which is never newer than its
	// real miss.
	missSeq     map[string]*missRecs
	missMax     int
	missWindow  time.Duration
	missBase    time.Time // origin of the missWindow/2 slices
	missSwept   time.Time // the last sweep of records with no live entry (sweepMissLocked)
	fallback    uint64
	fallbackAt  time.Time // the newest dropped entry's miss time; the fallback is live for missWindow after it
	hasFallback bool
	released    bool // reset ran: the reader is closed and records no more misses
}

// missRec is one miss: the applied seq and the time.
type missRec struct {
	seq uint64
	at  time.Time
}

// missRecs is one key's live misses, oldest first: at most one per missWindow/2 slice, and at most
// three slices are ever live (an entry older than missWindow is pruned).
type missRecs struct {
	r [3]missRec
	n int
}

// newLocal builds an empty local map; now is the reader's clock reading at construction (D-039).
func newLocal(maxEntries, missMax int, missWindow time.Duration, now time.Time) *local {
	return &local{
		entries:    make(map[string]localEntry),
		max:        maxEntries,
		missSeq:    make(map[string]*missRecs),
		missMax:    missMax,
		missWindow: missWindow,
		missBase:   now,
		missSwept:  now,
	}
}

// sweepMissLocked drops every miss record with no live entry, at most once per missWindow. A record
// with no live entry behaves exactly as an absent one (baseSeq deletes it and falls through; miss
// refills it as a new record), so dropping it changes no base_seq; a record with a live entry is
// only pruned of its dead entries, which baseSeq and miss do anyway. Without it, a key missed once
// and never read again kept its record until missMax keys accumulated (D-039 finding). Cost: one
// pass over at most missMax records per missWindow, on the first new-key miss after the window.
func (l *local) sweepMissLocked(now time.Time) {
	if now.Sub(l.missSwept) < l.missWindow {
		return
	}
	l.missSwept = now
	for k, rs := range l.missSeq {
		if !l.pruneLocked(rs, now) {
			delete(l.missSeq, k)
		}
	}
}

func (l *local) liveLocked(at, now time.Time) bool { return now.Sub(at) < l.missWindow }

func (l *local) sliceLocked(at time.Time) int64 {
	return int64(at.Sub(l.missBase) / max(l.missWindow/2, 1))
}

// pruneLocked drops rs's entries older than missWindow and reports whether any is left.
func (l *local) pruneLocked(rs *missRecs, now time.Time) bool {
	drop := 0
	for drop < rs.n && !l.liveLocked(rs.r[drop].at, now) {
		drop++
	}
	if drop > 0 {
		copy(rs.r[:], rs.r[drop:rs.n])
		rs.n -= drop
	}
	return rs.n > 0
}

// lookup returns the live entry for key.
func (l *local) lookup(key string, now time.Time) (localEntry, bool) {
	l.mu.RLock()
	e, ok := l.entries[key]
	l.mu.RUnlock()
	if !ok || !e.live(now) {
		return localEntry{}, false
	}
	return e, true
}

// miss records that key missed at the applied seq (R6). Within one missWindow/2 slice only the
// earliest miss is kept; a later miss never raises it. A set sends the oldest live entry, so a
// read-through whose miss-to-set time is under missWindow/2 always sends a base_seq at or before
// its own miss: the entry of its own slice is at most missWindow/2 older than its miss, so it is
// still live at the set. Entries older than missWindow are dropped, so after a covering write the
// key's read-through sets are refused for at most missWindow.
//
// New (no v1 counterpart), D-023: oldest miss, MissWindow, never consumed; one entry per
// half-window so a read-through that piggybacked on an expiring entry stays covered.
func (l *local) miss(key string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released {
		return
	}
	rs, ok := l.missSeq[key]
	if ok {
		l.pruneLocked(rs, now)
		if rs.n > 0 && l.sliceLocked(rs.r[rs.n-1].at) == l.sliceLocked(now) {
			return
		}
		if rs.n == len(rs.r) {
			copy(rs.r[:], rs.r[1:])
			rs.n--
		}
		rs.r[rs.n] = missRec{seq: l.seq, at: now}
		rs.n++
		return
	}
	l.sweepMissLocked(now)
	if len(l.missSeq) >= l.missMax {
		for k, rs := range l.missSeq {
			if !l.pruneLocked(rs, now) {
				delete(l.missSeq, k)
			}
		}
	}
	if len(l.missSeq) >= l.missMax {
		low, newest := l.seq, time.Time{}
		for _, rs := range l.missSeq {
			low = min(low, rs.r[0].seq)
			if at := rs.r[rs.n-1].at; at.After(newest) {
				newest = at
			}
		}
		if l.hasFallback && l.liveLocked(l.fallbackAt, now) {
			low = min(low, l.fallback)
			if l.fallbackAt.After(newest) {
				newest = l.fallbackAt
			}
		}
		clear(l.missSeq)
		l.fallback, l.fallbackAt, l.hasFallback = low, newest, true
	}
	rs = &missRecs{n: 1}
	rs.r[0] = missRec{seq: l.seq, at: now}
	l.missSeq[key] = rs
}

// baseSeq is the base_seq a set of key carries: its oldest live miss, else the live fallback after
// an overflow, else the applied seq (a write-through with no read before it). Nothing is consumed:
// a set, stored or refused, leaves the entries for the other read-throughs of the key.
func (l *local) baseSeq(key string, now time.Time) uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	if rs, ok := l.missSeq[key]; ok {
		if l.pruneLocked(rs, now) {
			return rs.r[0].seq
		}
		delete(l.missSeq, key)
	}
	if l.hasFallback && l.liveLocked(l.fallbackAt, now) {
		return min(l.fallback, l.seq)
	}
	return l.seq
}

func (l *local) bumpLocked(seq uint64) {
	if seq > l.seq {
		l.seq = seq
	}
}

func (l *local) putLocked(key string, raw json.RawMessage, expiresAt time.Time, now time.Time) {
	if len(raw) == 0 {
		delete(l.entries, key)
		return
	}
	if _, ok := l.entries[key]; !ok && len(l.entries) >= l.max {
		l.evictLocked(now)
	}
	l.entries[key] = localEntry{raw: raw, expiresAt: expiresAt}
}

// evictLocked makes room for one new key: an expired entry among the first evictScan, else the
// first one seen (Go randomises map iteration, so that is arbitrary). Evicting is a miss, never a
// stale read (D-008).
func (l *local) evictLocked(now time.Time) {
	var victim string
	found := false
	n := 0
	for k, e := range l.entries {
		if !e.live(now) {
			delete(l.entries, k)
			return
		}
		if !found {
			victim, found = k, true
		}
		if n++; n >= evictScan {
			break
		}
	}
	if found {
		delete(l.entries, victim)
	}
}

// put stores a value (a replicate broadcast or a set ack) and applies its seq.
func (l *local) put(key string, raw json.RawMessage, expiresAt time.Time, seq uint64, now time.Time) {
	l.mu.Lock()
	l.putLocked(key, raw, expiresAt, now)
	l.bumpLocked(seq)
	l.mu.Unlock()
}

// removeLocked applies one delete-shaped op. Prefix and suffix scan the local map; the server's
// index is the hot sweep path (R7), the reader holds one sheet.
func (l *local) removeLocked(kind opKind, arg string) {
	switch kind {
	case opDelete:
		delete(l.entries, arg)
	case opDeletePrefix:
		if arg == "" {
			l.entries = make(map[string]localEntry)
			return
		}
		for k := range l.entries {
			if strings.HasPrefix(k, arg) {
				delete(l.entries, k)
			}
		}
	case opDeleteSuffix:
		for k := range l.entries {
			if strings.HasSuffix(k, arg) {
				delete(l.entries, k)
			}
		}
	}
}

// shrinkLocked replaces an emptied map by a fresh one: a Go map never shrinks on delete, so a map
// drained key by key would keep its peak bucket array for the reader's life. Caller holds mu.
func (l *local) shrinkLocked() {
	if len(l.entries) == 0 {
		l.entries = make(map[string]localEntry)
	}
}

// remove applies a delete-shaped op under one lock pass and applies its seq.
func (l *local) remove(kind opKind, arg string, seq uint64) {
	l.mu.Lock()
	l.removeLocked(kind, arg)
	l.shrinkLocked()
	l.bumpLocked(seq)
	l.mu.Unlock()
}

// invalidate applies an invalidate broadcast: key, keys (janitor expiry), prefix and/or suffix.
//
// after advo_cache/reader/reader.go:501-522: prefix/suffix as strings ("" = nothing to do) →
// pointers, so prefix "" clears the sheet as the server did; keys[] is new (D-003).
func (l *local) invalidate(m protocol.Invalidate) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if m.Key != "" {
		delete(l.entries, m.Key)
	}
	for _, k := range m.Keys {
		delete(l.entries, k)
	}
	if m.Prefix != nil {
		l.removeLocked(opDeletePrefix, *m.Prefix)
	}
	if m.Suffix != nil {
		l.removeLocked(opDeleteSuffix, *m.Suffix)
	}
	l.shrinkLocked()
	l.bumpLocked(m.Seq)
}

// replace loads a sheet_state: the map is replaced, the applied seq is SET to the state's seq (a
// new server's numbering is the only valid base for base_seq), and every recorded miss becomes 0
// (its time kept), which the server treats conservatively (R6).
//
// after advo_cache/reader/reader.go:443-454: clear + reload of decoded values → one swap of raw
// bytes with expiry, bounded by max.
func (l *local) replace(s protocol.SheetState, now time.Time) {
	m := make(map[string]localEntry, min(len(s.Data), l.max))
	for k, raw := range s.Data {
		if len(m) >= l.max {
			break
		}
		if len(raw) == 0 {
			continue
		}
		m[k] = localEntry{raw: raw, expiresAt: expiry(now, s.ExpiresInMS[k])}
	}
	l.mu.Lock()
	l.entries = m
	l.seq = s.Seq
	for _, rs := range l.missSeq {
		for i := range rs.n {
			rs.r[i].seq = 0
		}
	}
	if l.hasFallback {
		l.fallback = 0
	}
	l.mu.Unlock()
}

// reset frees the map, the miss records and the fallback: the reader is closed and only misses.
func (l *local) reset() {
	l.mu.Lock()
	l.entries = make(map[string]localEntry)
	l.missSeq = make(map[string]*missRecs)
	l.fallback, l.fallbackAt, l.hasFallback = 0, time.Time{}, false
	l.released = true
	l.mu.Unlock()
}

// counts returns len(entries) and len(missSeq) under one read lock.
func (l *local) counts() (entries, misses int) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return len(l.entries), len(l.missSeq)
}

func (l *local) len() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return len(l.entries)
}

func (l *local) appliedSeq() uint64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.seq
}
