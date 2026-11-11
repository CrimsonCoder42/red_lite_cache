// Covers: reader local-map index (S2, doc/reader_optimize_2026-11-10)
package reader

import (
	"encoding/json"
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/CrimsonCoder42/red_lite_cache/protocol"
)

// indexKeyPool is every key shape the old segment index treated differently, kept as edge cases
// for the ordered one: scoped keys (closed and open), records, keys with no ':', a leading or
// trailing ':', an empty id segment, "owner" as a root, and keys that are prefixes or suffixes of
// each other.
func indexKeyPool(rng *rand.Rand, n int) []string {
	roots := []string{"condition", "med", "pef", "user", "owner", "", "c"}
	views := []string{"list", "count", "list:2", "", "summary"}
	seen := map[string]bool{}
	var pool []string
	add := func(k string) {
		if k != "" && !seen[k] {
			seen[k] = true
			pool = append(pool, k)
		}
	}
	// ids: enough distinct values that n keys exist (40 left ~2 300 possible keys and n = 3 000
	// never finished); few enough that keys share roots, heads and tails.
	ids := max(40, n/4)
	for len(pool) < n {
		root, view := roots[rng.IntN(len(roots))], views[rng.IntN(len(views))]
		id := fmt.Sprintf("%x", rng.IntN(ids))
		switch rng.IntN(8) {
		case 0, 1, 2:
			add(root + ":owner:" + id + ":" + view)
		case 3:
			add(root + ":owner:" + id)
		case 4:
			add(root + ":record:" + id)
		case 5:
			add(root + id) // no ':'
		case 6:
			add(":" + root + ":" + view)
		default:
			add(root + ":owner::" + view)
		}
	}
	return pool
}

// randomCut is a random prefix (fromEnd false) or suffix of a random pool key.
func randomCut(rng *rand.Rand, pool []string, fromEnd bool) string {
	k := pool[rng.IntN(len(pool))]
	i := rng.IntN(len(k) + 1)
	if fromEnd {
		return k[i:]
	}
	return k[:i]
}

// TestLocalIndexMatchesScan (S2 a): 120 000 random puts, deletes, prefix and suffix deletes and
// invalidate frames on the indexed local map and on a plain map swept by strings.HasPrefix /
// HasSuffix. After every op the sizes match; every 1 000 ops and at the end the key sets match
// and every index bucket count equals the count computed from the keys.
func TestLocalIndexMatchesScan(t *testing.T) {
	const ops = 120_000
	seed := uint64(time.Now().UnixNano())
	t.Logf("seed %d", seed)
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	pool := indexKeyPool(rng, 3_000)
	now := time.Now()
	l := newLocal(len(pool)+1, missSeqMax, DefaultMissWindow, now) // no eviction: the model has none
	model := map[string]bool{}
	raw := json.RawMessage(`1`)

	modelSweep := func(match func(string) bool) {
		for k := range model {
			if match(k) {
				delete(model, k)
			}
		}
	}
	for i := range ops {
		var what string
		switch op := rng.IntN(100); {
		case op < 55:
			k := pool[rng.IntN(len(pool))]
			what = "put " + k
			l.put(k, raw, time.Time{}, 0, now)
			model[k] = true
		case op < 70:
			k := pool[rng.IntN(len(pool))]
			what = "delete " + k
			l.remove(opDelete, k, 0)
			delete(model, k)
		case op < 82:
			p := randomCut(rng, pool, false)
			if p == "" && rng.IntN(20) != 0 { // a whole-sheet clear, rarely
				p = pool[rng.IntN(len(pool))][:1]
			}
			what = "delete_prefix " + p
			l.remove(opDeletePrefix, p, 0)
			modelSweep(func(k string) bool { return strings.HasPrefix(k, p) })
		case op < 92:
			s := randomCut(rng, pool, true)
			if s == "" && rng.IntN(20) != 0 {
				k := pool[rng.IntN(len(pool))]
				s = k[len(k)-1:]
			}
			what = "delete_suffix " + s
			l.remove(opDeleteSuffix, s, 0)
			modelSweep(func(k string) bool { return strings.HasSuffix(k, s) })
		default: // an invalidate frame: key, keys and a prefix or suffix together
			m := protocol.Invalidate{Type: protocol.TypeInvalidate, Key: pool[rng.IntN(len(pool))],
				Keys: []string{pool[rng.IntN(len(pool))], pool[rng.IntN(len(pool))]}}
			if rng.IntN(2) == 0 {
				m.Prefix = protocol.Ptr(randomCut(rng, pool, false) + "x")
			} else {
				m.Suffix = protocol.Ptr("x" + randomCut(rng, pool, true))
			}
			what = fmt.Sprintf("invalidate %+v", m)
			l.invalidate(m)
			delete(model, m.Key)
			for _, k := range m.Keys {
				delete(model, k)
			}
			if m.Prefix != nil {
				modelSweep(func(k string) bool { return strings.HasPrefix(k, *m.Prefix) })
			}
			if m.Suffix != nil {
				modelSweep(func(k string) bool { return strings.HasSuffix(k, *m.Suffix) })
			}
		}
		if got := l.len(); got != len(model) {
			t.Fatalf("op %d (%s): local holds %d keys, scan model %d", i, what, got, len(model))
		}
		if i%1_000 == 0 || i == ops-1 {
			compareLocal(t, l, model, fmt.Sprintf("op %d (%s)", i, what))
		}
	}
}

// compareLocal fails unless l holds exactly model's keys, in its map, its eviction slots and its
// index (cache.Groups, in order), and the index holds nodes exactly when the map holds keys.
func compareLocal(t *testing.T, l *local, model map[string]bool, at string) {
	t.Helper()
	l.mu.RLock()
	got := slices.Sorted(maps.Keys(l.entries))
	if len(l.slots) != len(l.entries) { // the eviction slots (D-043) mirror entries
		t.Fatalf("%s: slots hold %d, map %d", at, len(l.slots), len(l.entries))
	}
	for k, e := range l.entries {
		s := l.slots[e.pos]
		if s.key != k {
			t.Fatalf("%s: slots[%d] = %q, want %q", at, e.pos, s.key, k)
		}
		if want := newSlot(k, e.expiresAt, time.Time{}).expires; s.expires != want {
			t.Fatalf("%s: slot %q deadline %d, entry %d", at, k, s.expires, want)
		}
	}
	l.mu.RUnlock()
	want := slices.Sorted(maps.Keys(model))
	if !slices.Equal(got, want) {
		t.Fatalf("%s: key sets differ: local %d keys, model %d", at, len(got), len(want))
	}
	l.mu.RLock()
	indexed := l.groups.Scan(nil, "", len(want)+1)
	n := l.groups.Len()
	l.mu.RUnlock()
	if !slices.Equal(indexed, want) || n != len(want) {
		t.Fatalf("%s: index holds %d keys (scan %d), map %d", at, n, len(indexed), len(want))
	}
	if st := l.stats(); (st.ForwardNodes == 0) != (len(want) == 0) || (st.ReverseNodes == 0) != (len(want) == 0) {
		t.Fatalf("%s: index nodes %d/%d for %d keys", at, st.ForwardNodes, st.ReverseNodes, len(want))
	}
}

// TestLocalIndexDrainsToZero (S2 d, unit half): a filled map drained by each removal path — exact
// deletes, prefix and suffix deletes, invalidate keys, eviction and a sheet_state — ends with 0
// entries and 0 index nodes, and Reader.Stats reports them.
func TestLocalIndexDrainsToZero(t *testing.T) {
	now := time.Now()
	keys := benchKeys(5_000)
	raw := json.RawMessage(`1`)
	fill := func(l *local) {
		for _, k := range keys {
			l.put(k, raw, time.Time{}, 0, now)
		}
		if st := l.stats(); st.ForwardNodes == 0 || st.ReverseNodes == 0 {
			t.Fatalf("filled map has empty indexes: %+v", st)
		}
	}
	zero := func(l *local, how string) {
		t.Helper()
		if st := l.stats(); st != (localStats{}) {
			t.Fatalf("after %s: %+v, want all 0", how, st)
		}
	}
	l := newLocal(len(keys)+1, missSeqMax, DefaultMissWindow, now)

	fill(l)
	for _, k := range keys {
		l.remove(opDelete, k, 0)
	}
	zero(l, "exact deletes")

	fill(l)
	for _, r := range benchRoots {
		l.remove(opDeletePrefix, r+":", 0)
	}
	l.remove(opDeleteSuffix, keys[4][len("record"):], 0) // one record key, then the rest by root
	l.remove(opDeletePrefix, "record", 0)
	zero(l, "prefix and suffix deletes")

	fill(l)
	l.invalidate(protocol.Invalidate{Type: protocol.TypeInvalidate, Keys: keys})
	zero(l, "invalidate keys")

	fill(l)
	l.replace(protocol.SheetState{Type: protocol.TypeSheetState}, now)
	zero(l, "an empty sheet_state")

	small := newLocal(10, missSeqMax, DefaultMissWindow, now) // eviction keeps the index exact
	for _, k := range keys {
		small.put(k, raw, time.Time{}, 0, now)
	}
	if st := small.stats(); st.Entries != 10 {
		t.Fatalf("bounded map holds %d", st.Entries)
	}
	left := map[string]bool{}
	for k := range small.entries {
		left[k] = true
	}
	compareLocal(t, small, left, "eviction churn")
	small.remove(opDeletePrefix, "", 0)
	zero(small, "eviction churn then a clear")

	f := newFakeServer(t)
	r := mustNew(t, testConfig(f.addr()))
	st := r.Stats()
	if st.ForwardNodes != 0 || st.ReverseNodes != 0 {
		t.Fatalf("empty reader Stats = %+v", st)
	}
}
