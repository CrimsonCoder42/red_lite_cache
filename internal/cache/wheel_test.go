package cache

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"
	"time"
)

// TestWheelDueExact: a pass removes exactly the keys whose live expiry is <= now — including keys
// due earlier in the current second — keeps the rest of that second for the next pass, skips
// stale items (re-set or deleted keys), and handles an item added for a second already passed.
func TestWheelDueExact(t *testing.T) {
	base := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	exp := map[string]time.Time{}
	w := NewWheel()
	add := func(k string, at time.Time) { exp[k] = at; w.Add(k, at) }
	add("a", base.Add(100*time.Millisecond))
	add("b", base.Add(900*time.Millisecond))  // same second as a, later
	add("c", base.Add(2500*time.Millisecond)) // two seconds on
	add("d", base.Add(1500*time.Millisecond))
	add("d", base.Add(5*time.Second)) // re-set: the first item is stale
	add("gone", base.Add(200*time.Millisecond))
	delete(exp, "gone") // deleted: its item is stale
	live := func(k string) (time.Time, bool) { at, ok := exp[k]; return at, ok }
	pass := func(now time.Time) []string {
		var got []string
		w.Due(now, live, func(k string) { got = append(got, k); delete(exp, k) })
		slices.Sort(got)
		return got
	}
	if got := pass(base.Add(500 * time.Millisecond)); !slices.Equal(got, []string{"a"}) {
		t.Fatalf("pass at +0.5s = %v, want [a] (b is later in the same second)", got)
	}
	if got := pass(base.Add(900 * time.Millisecond)); !slices.Equal(got, []string{"b"}) {
		t.Fatalf("pass at +0.9s = %v, want [b]", got)
	}
	if got := pass(base.Add(3 * time.Second)); !slices.Equal(got, []string{"c"}) {
		t.Fatalf("pass at +3s = %v, want [c] (d was re-set to +5s)", got)
	}
	add("late", base.Add(time.Second)) // a second the wheel has already passed
	if got := pass(base.Add(3 * time.Second)); !slices.Equal(got, []string{"late"}) {
		t.Fatalf("late add = %v, want [late]", got)
	}
	if got := pass(base.Add(10 * time.Second)); !slices.Equal(got, []string{"d"}) {
		t.Fatalf("pass at +10s = %v, want [d]", got)
	}
	if st := w.Stats(); st != (WheelStats{}) {
		t.Fatalf("drained wheel keeps %+v", st)
	}
}

// TestWheelCompaction (S9 criterion 8, D-045): 100,000 overwrites of 1,000 expiring keys; after
// every 1,000 the wheel holds at most 2 × live items, so overwrites cannot grow it without bound.
func TestWheelCompaction(t *testing.T) {
	s := New()
	rng := rand.New(rand.NewPCG(1, 1))
	base := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	for i := range 100_000 {
		k := fmt.Sprintf("k:%03d", i%1000)
		s.Set(k, Entry{Value: val("v"), ExpiresAt: base.Add(time.Duration(1+rng.IntN(3600)) * time.Second)})
		if i%1000 == 999 {
			if st := s.Stats(); st.WheelItems > 2*st.Entries {
				t.Fatalf("after %d sets: %d wheel items for %d live keys", i+1, st.WheelItems, st.Entries)
			}
		}
	}
	// and the compacted wheel still expires every key on time
	if got := len(s.Expire(base.Add(2 * time.Hour))); got != 1000 {
		t.Fatalf("Expire after compaction removed %d, want 1000", got)
	}
	if st := s.Stats(); st != (Stats{}) {
		t.Fatalf("drained store keeps %+v", st)
	}
}
