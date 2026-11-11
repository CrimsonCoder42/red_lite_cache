package sheet

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/CrimsonCoder42/red_lite_cache/internal/cache"
)

// fillStore puts n keys straight into s's store, expiring one per second over a day from exp, so
// the wheel has many slots and a pass walks its seconds (not the few-slots map branch).
func fillStore(s *Sheet, n int, exp time.Time) {
	for i := range n {
		at := exp.Add(time.Duration(i%86_400) * time.Second)
		s.store.Set(fmt.Sprintf("k:%06d", i), cache.Entry{Value: json.RawMessage(`1`), ExpiresAt: at})
	}
}

// idlePass is the shortest of five janitor passes over s with nothing due, each under the sheet
// mutex as the janitor holds it (Sheet.expire), one minute (the default interval) apart.
func idlePass(s *Sheet, clk *clock) time.Duration {
	best := time.Duration(1 << 62)
	for range 5 {
		clk.Advance(time.Minute)
		t0 := time.Now()
		if n := s.expire(clk.Now()); n != 0 {
			panic(fmt.Sprintf("idle pass expired %d", n))
		}
		best = min(best, time.Since(t0))
	}
	return best
}

// TestWheelIdlePassHoldTime (S9 criterion 7, D-045): a janitor pass with nothing due costs the
// same on a 100,000-key sheet as on a 1,000-key one — it no longer looks at every entry while the
// sheet mutex is held. Compared as a ratio, so -race and machine speed cancel out; the absolute
// time is a benchmark target (doc/ordered_index_build_2026-11-11/REPORT.md).
func TestWheelIdlePassHoldTime(t *testing.T) {
	r, clk := newTestRegistry(t)
	big, small := openSheet(t, r, "big"), openSheet(t, r, "small")
	exp := clk.Now().Add(24 * time.Hour)
	fillStore(big, 100_000, exp)
	fillStore(small, 1_000, exp)
	b, s := idlePass(big, clk), idlePass(small, clk)
	t.Logf("idle pass: 100,000 keys %v, 1,000 keys %v", b, s)
	if b > 5*s+50*time.Microsecond {
		t.Fatalf("idle pass grows with the sheet: 100,000 keys %v vs 1,000 keys %v", b, s)
	}
}

// TestReplicaWheelOnPromotion (S9 criterion 9, D-045): a replica files every replicated expiry in
// its wheel as it applies the stream (no janitor pass runs there), so on promotion the first pass
// expires the keys on schedule with no rebuild.
func TestReplicaWheelOnPromotion(t *testing.T) {
	r, clk := newTestRegistry(t)
	s := r.Restore(DefaultProject, "rep", nil)
	exp := clk.Now().Add(time.Minute)
	for i := range 1000 {
		op := Op{Kind: OpSet, Seq: uint64(i + 1), Key: fmt.Sprintf("k:%04d", i), Value: json.RawMessage(`1`)}
		if i%2 == 0 {
			op.ExpiresAt = exp
		}
		s.ApplyReplicated(op)
	}
	if st := s.StoreStats(); st.WheelItems != 500 || st.Entries != 1000 {
		t.Fatalf("replica before promotion: %+v, want 500 wheel items for 1000 entries", st)
	}
	clk.Advance(2 * time.Minute)
	if n := r.ExpireAll(clk.Now()); n != 500 {
		t.Fatalf("first pass after promotion expired %d, want 500", n)
	}
	if st := s.StoreStats(); st.Entries != 500 || st.WheelItems != 0 {
		t.Fatalf("after the pass: %+v", st)
	}
}
