package reader

import (
	"bytes"
	"encoding/json"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/CrimsonCoder42/red_lite_cache/internal/cache"
	"github.com/CrimsonCoder42/red_lite_cache/protocol"
)

// evictSample is how many entries a put on a full map looks at (D-043, sampled LRU the Redis way):
// the first expired one among them is evicted, else the least recently read. A fixed sample keeps
// the put O(1); 5 is Redis's default maxmemory-samples.
const evictSample = 5

// accessResolution is how far apart (ms) two reads of one key must be for the second to move its
// last-access time; in between, a Get only loads. LRU holds while the whole map takes longer than
// this to turn over: faster, and keys stored since a hot key's last recorded read look newer than
// it. Measured both ways (doc/reader_optimize_2026-11-10/report.md, S3): 1 000 ms (Redis's LRU
// clock) kept 0 of 100 hot keys at a 1 ms insert step (TestEvictionKeepsHotKeys/1ms, checker
// 2026-11-10); 1 ms made parallel Get at 100 000 entries 22% slower than HEAD (a store on nearly
// every Get). 100 ms holds LRU up to 10 turnovers of the map a second.
const accessResolution = 100

// localEntry is one cached value: the JSON bytes exactly as the server sent them, never mutated
// after they are stored, the local deadline derived from the server's expires_in_ms, and pos, the
// index of the key's slot in local.slots (D-043).
type localEntry struct {
	raw       json.RawMessage
	expiresAt time.Time // zero = no expiry
	pos       int
}

func (e localEntry) live(now time.Time) bool {
	return e.expiresAt.IsZero() || now.Before(e.expiresAt)
}

func (e localEntry) isNull() bool { return bytes.Equal(e.raw, protocol.NullValue) }

// slot is one key's eviction data (D-043), kept apart from the map so a draw reads one array
// element instead of a map lookup and an entry. access is written by Get under the READ lock, so it
// is only touched through sync/atomic there; under the write lock (put, delete, evict) the whole
// slot is read and written plainly — no reader runs then.
type slot struct {
	key     string
	access  int64 // last read or store, Unix milliseconds of the reader's clock
	expires int64 // the entry's deadline, Unix nanoseconds; 0 = none
}

func newSlot(key string, expiresAt, now time.Time) slot {
	s := slot{key: key, access: now.UnixMilli()}
	if !expiresAt.IsZero() {
		s.expires = expiresAt.UnixNano()
	}
	return s
}

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
// map + RWMutex of raw bytes with a deadline, bounded by max with sampled-LRU eviction (D-002,
// D-008, D-043), with the server store's own index (cache.Groups, D-044: the same package, so both
// sides decide a group identically) so a prefix or suffix delete visits only the keys it matches
// (R7) — its own deletes and every one the server broadcasts.
type local struct {
	mu sync.RWMutex
	// Every RLock/RUnlock writes mu's reader count, so the cache line holding mu bounces between
	// CPUs on parallel Gets; a field sharing that line costs a coherence miss on every read under
	// the lock. Get reads entries and slots, so mu gets a line of its own (128 bytes: Apple M-series
	// lines; two x86 lines). Measured: without it, parallel Get was 20-26% slower than HEAD; with
	// it, level (doc/reader_optimize_2026-11-10/report.md, S3).
	_       [128]byte
	entries map[string]localEntry
	max     int
	// groups indexes exactly the keys of entries; every add and remove of a key goes through
	// putLocked, deleteLocked, removeLocked (the index cuts a group out, dropLocked removes its
	// entries), clearLocked or replace, which keep the two in step.
	groups *cache.Groups
	// wheel files every entry with a deadline under its second, the same cache.Wheel as the
	// server's store (D-045): expirePass drops due entries without looking at the others. expiring
	// counts the entries with a deadline (the wheel's live items, which bound its stale ones).
	wheel    *cache.Wheel
	expiring int
	// victims is the reused buffer for one prefix/suffix delete's matches (under mu).
	victims []string
	// slots holds one slot per key of entries, in no order (entries[k].pos is its index), so
	// eviction can draw uniformly random entries in O(1) (D-043). Kept in step with entries by
	// putLocked, dropLocked (swap-remove), clearLocked and replace.
	slots []slot
	// draw picks an index in [0, n) for eviction: rand.IntN, or a fixed sequence in an in-package
	// test (the D-039 seam: unexported, never configurable).
	draw func(n int) int

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
		groups:     cache.NewGroups(),
		wheel:      cache.NewWheel(),
		draw:       rand.IntN,
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

// lookup returns the live entry for key and records the read for eviction (D-043). It takes only
// the read lock: touch is an atomic store into the entry. The entry's raw bytes are never mutated,
// so the caller may read them after the lock is released.
func (l *local) lookup(key string, now time.Time) (localEntry, bool) {
	l.mu.RLock()
	e, ok := l.entries[key]
	if ok && e.live(now) {
		// The read is recorded while the read lock pins e.pos (a delete's swap-remove moves slots
		// only under the write lock), at accessResolution: a key read often stores once per
		// resolution and otherwise only loads, so Gets on neighbouring slots from other CPUs do
		// not keep invalidating each other's cache line.
		a := &l.slots[e.pos].access
		if ms := now.UnixMilli(); ms-atomic.LoadInt64(a) >= accessResolution {
			atomic.StoreInt64(a, ms)
		}
	}
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
		l.deleteLocked(key)
		return
	}
	// A store counts as an access (D-043).
	e := localEntry{raw: raw, expiresAt: expiresAt}
	if old, ok := l.entries[key]; ok {
		e.pos = old.pos
		l.slots[e.pos] = newSlot(key, expiresAt, now)
		if !old.expiresAt.IsZero() {
			l.expiring-- // its wheel item goes stale
		}
	} else {
		if len(l.entries) >= l.max {
			l.evictLocked(now)
		}
		l.groups.Add(key)
		e.pos = len(l.slots)
		l.slots = append(l.slots, newSlot(key, expiresAt, now))
	}
	l.entries[key] = e
	if !expiresAt.IsZero() {
		l.expiring++
		l.wheel.Add(key, expiresAt)
		l.wheel.Compact(l.expiring, l.expiryOf)
	}
}

// expiryOf is the wheel's view of key: its deadline, false when it is gone. Caller holds mu.
func (l *local) expiryOf(key string) (time.Time, bool) {
	e, ok := l.entries[key]
	return e.expiresAt, ok
}

// expirePass drops every entry whose deadline is <= now (D-045), the reader's half of R4/R5: a read
// already misses such an entry; the pass frees it without waiting for the server's invalidate{keys}
// (so also while disconnected). It walks only the wheel's due seconds. Returns how many it dropped.
func (l *local) expirePass(now time.Time) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	l.wheel.Due(now, l.expiryOf, func(k string) {
		l.deleteLocked(k)
		n++
	})
	l.shrinkLocked()
	return n
}

// deleteLocked removes key from the map, the index and slots. Caller holds mu.
func (l *local) deleteLocked(key string) {
	if _, ok := l.entries[key]; !ok {
		return
	}
	l.groups.Remove(key)
	l.dropLocked(key)
}

// dropLocked removes a present key from the map and slots (the last slot moves into its place) but
// not from the index: the caller already removed it there (removeLocked: the index cut its whole
// group out). Caller holds mu.
func (l *local) dropLocked(key string) {
	e := l.entries[key]
	if !e.expiresAt.IsZero() {
		l.expiring-- // its wheel item goes stale
	}
	delete(l.entries, key)
	last := len(l.slots) - 1
	if e.pos != last {
		moved := l.slots[last]
		l.slots[e.pos] = moved
		me := l.entries[moved.key]
		me.pos = e.pos
		l.entries[moved.key] = me
	}
	l.slots[last] = slot{} // the backing array must not keep the key alive
	l.slots = l.slots[:last]
}

// clearLocked empties the map, the index and slots with fresh ones (a Go map never shrinks on
// delete). Caller holds mu.
func (l *local) clearLocked() {
	l.entries = make(map[string]localEntry)
	l.groups.Reset()
	l.wheel.Reset()
	l.expiring = 0
	l.slots = nil
}

// evictLocked makes room for one new key (D-043): it draws evictSample entries and evicts the
// first expired one, else the least recently read of them. Evicting is a miss, never a stale read
// (D-008). Caller holds mu.
//
// Draws are uniform over slots, not taken from a map range. A range starts at a random position
// but returns the next full one, so it favours entries next to empty map slots — and under LRU
// those are exactly the hot keys that survive while their evicted neighbours leave holes. Measured
// (probe 2026-11-10, TestEvictionKeepsHotKeys, 1 000 entries, 100 hot): one 5-entry range window
// kept 90 of 100 hot keys; five 1-entry ranges kept 96-99; uniform draws kept 100 in 5 runs.
//
// A draw reads only its slot (key, access, deadline): no map lookup. The deadline check compares
// wall-clock nanoseconds, where live compares time.Time (monotonic when both have it); the two can
// differ only at the boundary, and this check only picks a victim — reads still use live.
func (l *local) evictLocked(now time.Time) {
	if len(l.slots) == 0 {
		return
	}
	nowNS := now.UnixNano()
	victim := -1
	for range evictSample {
		i := l.draw(len(l.slots))
		s := &l.slots[i]
		if s.expires != 0 && nowNS >= s.expires {
			l.deleteLocked(s.key)
			return
		}
		if victim < 0 || s.access < l.slots[victim].access {
			victim = i
		}
	}
	l.deleteLocked(l.slots[victim].key)
}

// put stores a value (a replicate broadcast or a set ack) and applies its seq.
func (l *local) put(key string, raw json.RawMessage, expiresAt time.Time, seq uint64, now time.Time) {
	l.mu.Lock()
	l.putLocked(key, raw, expiresAt, now)
	l.bumpLocked(seq)
	l.mu.Unlock()
}

// maxVictimsBuf is the largest victims buffer kept between deletes; a bigger one (a sweep of most of
// the sheet) is dropped so one rare sweep does not pin its slice for the reader's life.
const maxVictimsBuf = 1024

// removeLocked applies one delete-shaped op. Prefix and suffix deletes have the index cut their
// group out (cache.Groups: one descent plus the matches), exactly as the server's store does; ""
// clears the sheet. Caller holds mu.
func (l *local) removeLocked(kind opKind, arg string) {
	switch kind {
	case opDelete:
		l.deleteLocked(arg)
		return
	case opDeletePrefix, opDeleteSuffix:
		if arg == "" { // every key starts and ends with ""
			l.clearLocked()
			return
		}
	}
	if kind == opDeletePrefix {
		l.victims = l.groups.CutPrefix(l.victims[:0], arg)
	} else {
		l.victims = l.groups.CutSuffix(l.victims[:0], arg)
	}
	for _, k := range l.victims {
		l.dropLocked(k)
	}
	clear(l.victims) // drop the key strings the buffer still points at
	if cap(l.victims) > maxVictimsBuf {
		l.victims = nil
	}
}

// shrinkLocked replaces an emptied map and index by fresh ones: a Go map never shrinks on delete,
// so a map drained key by key would keep its peak bucket array for the reader's life. Caller holds
// mu.
func (l *local) shrinkLocked() {
	if len(l.entries) == 0 {
		l.clearLocked()
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
		l.deleteLocked(m.Key)
	}
	for _, k := range m.Keys {
		l.deleteLocked(k)
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
// bytes with expiry, bounded by max. The new map and its index are built before the lock (the index
// in one pass from the keys, Groups.Load), so a large sheet_state blocks reads only for the swap.
func (l *local) replace(s protocol.SheetState, now time.Time) {
	n := min(len(s.Data), l.max)
	m := make(map[string]localEntry, n)
	slots := make([]slot, 0, n)
	keys := make([]string, 0, n)
	wheel, expiring := cache.NewWheel(), 0
	for k, raw := range s.Data {
		if len(m) >= l.max {
			break
		}
		if len(raw) == 0 {
			continue
		}
		exp := expiry(now, s.ExpiresInMS[k])
		m[k] = localEntry{raw: raw, expiresAt: exp, pos: len(slots)}
		slots = append(slots, newSlot(k, exp, now))
		keys = append(keys, k)
		if !exp.IsZero() {
			wheel.Add(k, exp)
			expiring++
		}
	}
	groups := cache.NewGroups()
	groups.Load(keys)
	l.mu.Lock()
	l.entries, l.groups, l.slots = m, groups, slots
	l.wheel, l.expiring = wheel, expiring
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
	l.clearLocked()
	l.victims = nil
	l.missSeq = make(map[string]*missRecs)
	l.fallback, l.fallbackAt, l.hasFallback = 0, time.Time{}, false
	l.released = true
	l.mu.Unlock()
}

// localStats is the local map's counts (D-040), taken under one read lock.
type localStats struct {
	Entries, Misses            int
	ForwardNodes, ReverseNodes int
	WheelItems                 int
}

func (l *local) stats() localStats {
	l.mu.RLock()
	defer l.mu.RUnlock()
	g := l.groups.Stats()
	return localStats{Entries: len(l.entries), Misses: len(l.missSeq),
		ForwardNodes: g.ForwardNodes, ReverseNodes: g.ReverseNodes, WheelItems: l.wheel.Stats().Items}
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
