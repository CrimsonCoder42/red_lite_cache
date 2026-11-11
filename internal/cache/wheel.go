package cache

import "time"

// Wheel finds the keys due to expire without looking at the others (S9 §4, D-045): every key
// with an expiry is filed under the second it expires in (unix seconds), and a pass walks only the
// seconds since the last pass. The server's Store and the reader's local map both use it, so both
// sides expire alike.
//
// Overwrites and deletes never search the wheel: the old item stays and is recognised as stale at
// pass time (the key is gone, has no expiry, or expires in another second). Compact bounds the
// stale items (D-045: rebuilt above 2 × live). Not safe for concurrent use: the owner's lock guards
// it.
//
// A pass is exact: it expires every key whose live expiry is <= now, including keys due earlier in
// the current second; the rest of that second is kept for the next pass.
type Wheel struct {
	slots map[int64][]string // unix second → keys filed under it
	// last: every second <= last has been processed (its slot is gone). A key filed under such a
	// second (an expiry already in the past when it was added) goes on late as well, because a pass
	// only walks forward from last.
	last  int64
	late  []int64
	items int // filed keys, stale ones included
}

// noneProcessed is last before the first pass: below every real second, far enough from the int64
// minimum that bound-last cannot overflow.
const noneProcessed = -1 << 62

// NewWheel returns an empty wheel.
func NewWheel() *Wheel { return &Wheel{slots: map[int64][]string{}, last: noneProcessed} }

// Reset empties the wheel and frees its slots.
func (w *Wheel) Reset() {
	w.slots = map[int64][]string{}
	w.late, w.items = nil, 0
}

// Add files key under at's second. at must not be the zero time.
func (w *Wheel) Add(key string, at time.Time) {
	s := at.Unix()
	if s <= w.last {
		w.late = append(w.late, s)
	}
	w.slots[s] = append(w.slots[s], key)
	w.items++
}

// Due runs one pass at now: every filed key whose live expiry (expiry(key), false = gone) is <= now
// is handed to expire, which must remove it before the next call; stale items are dropped. expire
// may not call back into the wheel.
func (w *Wheel) Due(now time.Time, expiry func(string) (time.Time, bool), expire func(string)) {
	// Keys are filed by their wall-clock second, so due is decided on the wall clock too: Round(0)
	// strips the monotonic reading, which time.Before would otherwise prefer. With it, a wall clock
	// stepped ahead of the monotonic one (NTP, a suspended host) left a key "not due" in a second
	// the walk had passed, never to be visited again (checker 2026-11-11; TestWheelWallClockJump).
	// Reads are unaffected: Get still decides expiry with the entry's own comparison (R4).
	now = now.Round(0)
	bound := now.Unix()
	for _, s := range w.late {
		w.process(s, now, expiry, expire)
	}
	w.late = w.late[:0]
	if bound-w.last > int64(len(w.slots)) {
		// More seconds than slots to walk (the first pass, or a long gap): visit the slots instead.
		for s := range w.slots {
			if s <= bound {
				w.process(s, now, expiry, expire)
			}
		}
	} else {
		for s := w.last + 1; s <= bound; s++ {
			if _, ok := w.slots[s]; ok {
				w.process(s, now, expiry, expire)
			}
		}
	}
	w.last = bound - 1 // the current second is revisited: some of its keys may not be due yet
}

// process walks one slot: due keys go to expire, stale items are dropped, keys due later in the
// same second stay.
func (w *Wheel) process(s int64, now time.Time, expiry func(string) (time.Time, bool), expire func(string)) {
	items, ok := w.slots[s]
	if !ok {
		return
	}
	keep := items[:0]
	for _, k := range items {
		at, live := expiry(k)
		switch {
		case !live || at.IsZero() || at.Unix() != s:
			w.items-- // stale: deleted, or re-set to another second (filed there too)
		case !now.Before(at.Round(0)): // now is already wall-clock only (Due)
			w.items--
			expire(k)
		default:
			keep = append(keep, k)
		}
	}
	clear(items[len(keep):]) // the backing array must not keep dropped keys alive
	if len(keep) == 0 {
		delete(w.slots, s)
	} else {
		w.slots[s] = keep
	}
}

// Compact drops the wheel's stale items when it holds more than 2 × live (D-045): live is the
// owner's count of keys with an expiry. Each live key is kept once, under its own second, filtered
// in place (no new map). A live key's items can only repeat within its own second, so duplicates
// are found inside one slot. The cost is O(items), at most once per live Adds: amortised O(1).
// Reports whether it ran.
func (w *Wheel) Compact(live int, expiry func(string) (time.Time, bool)) bool {
	if w.items <= 2*live {
		return false
	}
	n := 0
	w.late = w.late[:0]
	var seen map[string]struct{} // for a large slot only (a bulk fill with one TTL lands in one second)
	for s, items := range w.slots {
		keep := items[:0]
		big := len(items) > smallSlot
		if big {
			if seen == nil {
				seen = make(map[string]struct{}, len(items))
			} else {
				clear(seen)
			}
		}
		for _, k := range items {
			at, ok := expiry(k)
			if !ok || at.IsZero() || at.Unix() != s {
				continue
			}
			if big {
				if _, dup := seen[k]; dup {
					continue
				}
				seen[k] = struct{}{}
			} else if containsKey(keep, k) {
				continue
			}
			keep = append(keep, k)
		}
		clear(items[len(keep):])
		if len(keep) == 0 {
			delete(w.slots, s)
			continue
		}
		w.slots[s] = keep
		n += len(keep)
		if s <= w.last {
			w.late = append(w.late, s)
		}
	}
	w.items = n
	return true
}

// smallSlot is the largest slot Compact de-duplicates by scanning (above it, a map).
const smallSlot = 16

// containsKey reports whether k is in keys (a slot of at most smallSlot items).
func containsKey(keys []string, k string) bool {
	for _, x := range keys {
		if x == k {
			return true
		}
	}
	return false
}

// WheelStats is the wheel's size: counts only (D-040). Both are 0 when the wheel is empty.
type WheelStats struct {
	Items int // filed keys, stale ones included
	Slots int // seconds with at least one filed key
}

// Stats counts the wheel.
func (w *Wheel) Stats() WheelStats { return WheelStats{Items: w.items, Slots: len(w.slots)} }
