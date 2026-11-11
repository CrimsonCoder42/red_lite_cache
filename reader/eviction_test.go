// Covers: reader eviction policy (S3, D-043, doc/reader_optimize_2026-11-10)
package reader

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// TestEvictionKeepsHotKeys (D-043): a map at its cap (1 000) holds 100 keys that are read after
// every insert; 10 000 new keys are then inserted on the injected clock (D-039), one per step.
// Sampled LRU must keep at least 95 of the 100 read keys at every step, down to 1 ms (the whole
// map turning over in 1 s; checker 2026-11-10: a 1 s access resolution kept 0 there); arbitrary
// eviction keeps almost none (each eviction hits a given key with probability 1/1 000, 10 000
// times). The map stays at its cap and its index stays exact.
func TestEvictionKeepsHotKeys(t *testing.T) {
	for _, step := range []time.Duration{time.Second, 10 * time.Millisecond, time.Millisecond} {
		t.Run(step.String(), func(t *testing.T) { evictionKeepsHotKeys(t, step) })
	}
}

func evictionKeepsHotKeys(t *testing.T, step time.Duration) {
	const (
		capacity = 1_000
		hotN     = 100
		inserts  = 10 * capacity
	)
	clock := time.Now()
	tick := func() time.Time { clock = clock.Add(step); return clock }
	l := newLocal(capacity, missSeqMax, DefaultMissWindow, clock)
	raw := json.RawMessage(`1`)
	hot := make([]string, hotN)
	for i := range hot {
		hot[i] = fmt.Sprintf("condition:owner:hot-%d:list", i)
		l.put(hot[i], raw, time.Time{}, 0, tick())
	}
	for i := range capacity - hotN {
		l.put(fmt.Sprintf("med:owner:cold-%d:list", i), raw, time.Time{}, 0, tick())
	}
	readHot := func(now time.Time) {
		for _, k := range hot {
			l.lookup(k, now)
		}
	}
	readHot(tick())
	for i := range inserts {
		now := tick()
		l.put(fmt.Sprintf("vitals:owner:new-%d:count", i), raw, time.Time{}, 0, now)
		readHot(now)
	}
	kept := 0
	for _, k := range hot {
		if _, ok := l.lookup(k, clock); ok {
			kept++
		}
	}
	t.Logf("hot keys kept after %d inserts at cap %d: %d of %d", inserts, capacity, kept, hotN)
	if n := l.len(); n != capacity {
		t.Fatalf("map holds %d, cap %d", n, capacity)
	}
	if kept < 95 {
		t.Fatalf("kept %d of %d read keys, want >= 95", kept, hotN)
	}
	left := map[string]bool{}
	for k := range l.entries {
		left[k] = true
	}
	compareLocal(t, l, left, "after eviction churn")
}

// drawSeq replaces the eviction draw with a fixed sequence of key positions (positions follow
// insertion order until a delete swaps the last key into the freed slot).
func drawSeq(l *local, seq ...int) {
	i := 0
	l.draw = func(n int) int { p := seq[i%len(seq)] % n; i++; return p }
}

// TestEvictionSample (D-043) with a fixed draw: an expired entry in the sample is evicted even when
// a live one in the sample was read longer ago; with no expired entry in the sample, the least
// recently read of the sample goes; an entry outside the sample is never touched.
func TestEvictionSample(t *testing.T) {
	clock := time.Now()
	raw := json.RawMessage(`1`)
	l := newLocal(5, missSeqMax, DefaultMissWindow, clock)
	l.put("live-old", raw, time.Time{}, 0, clock)                   // pos 0: live, never read again
	l.put("expired", raw, clock.Add(time.Second), 0, clock.Add(10)) // pos 1: stored after live-old
	for i := 2; i < 5; i++ {
		l.put(fmt.Sprintf("k%d", i), raw, time.Time{}, 0, clock) // pos 2-4
	}
	clock = clock.Add(2 * time.Second)
	for i := 2; i < 5; i++ {
		l.lookup(fmt.Sprintf("k%d", i), clock) // read now: the most recent
	}

	drawSeq(l, 0, 0, 0, 0, 1) // the sample: live-old ×4, then expired
	l.put("new-a", raw, time.Time{}, 0, clock)
	if _, ok := l.lookup("live-old", clock); !ok {
		t.Fatal("evicted the live least-recently-read entry while the sample held an expired one")
	}
	if _, ok := l.entries["expired"]; ok {
		t.Fatal("the expired entry in the sample was not evicted")
	}

	// Now no entry is expired. Removing "expired" (pos 1) moved the last key, k4, into pos 1, and
	// new-a took pos 4: keys = live-old, k4, k2, k3, new-a. A sample of k2, k3, k4 (k3 read one
	// accessResolution after the other two) must take k2 — tied with k4, drawn first — and leave
	// live-old (pos 0, older than all of them) alone, because it was not drawn.
	clock = clock.Add(time.Second)
	l.lookup("k3", clock)
	drawSeq(l, 2, 3, 1, 2, 3)
	l.put("new-b", raw, time.Time{}, 0, clock)
	if _, ok := l.entries["k2"]; ok {
		t.Fatal("the least recently read entry of the sample was not evicted")
	}
	for _, k := range []string{"live-old", "k3", "k4", "new-a", "new-b"} {
		if _, ok := l.entries[k]; !ok {
			t.Fatalf("%s evicted although it was not the sample's oldest", k)
		}
	}
	left := map[string]bool{}
	for k := range l.entries {
		left[k] = true
	}
	compareLocal(t, l, left, "after sampled evictions") // also checks every slot against its entry
}
