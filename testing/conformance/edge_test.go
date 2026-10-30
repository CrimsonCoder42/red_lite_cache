// Covers: backendAdvo cache layer
package conformance

// Edge cases of the consumer contract: a key with no ':', a prefix equal to a whole key, the
// refused empty prefix, a cached value of another JSON shape, a cached zero value.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestEdgeKeyWithoutColon: a key with no ':' reads through, is shared, is untouched by a profile
// sweep, and an exact Delete removes it everywhere.
func TestEdgeKeyWithoutColon(t *testing.T) {
	cfForModes(t, func(t *testing.T, m cfMode) {
		c := cfStart(t, m)
		p := uuid.New()
		c.db.Put(p, Row{Profile: p, Value: 4})
		const key = "conformancenocolon"
		c.readList(c.st[0], key, p, TTLVitals)
		c.seenEverywhere(key, 4)
		c.readList(c.st[2], key, p, TTLVitals)
		c.expectLoads(1, "cached once")
		if err := c.st[1].InvalidateDomain(context.Background(), "condition", p); err != nil {
			t.Fatal(err)
		}
		c.expectPresence([]string{key}, nil, "a profile sweep")
		if err := c.st[1].R.Delete(context.Background(), key); err != nil {
			t.Fatal(err)
		}
		c.expectPresence([]string{key}, map[string]bool{key: true}, "exact delete")
	})
}

// TestEdgePrefixEqualsWholeKey: a key spelled exactly as the sweep prefix is removed by that
// sweep; DeletePrefix of a whole key removes that key and the keys it is a prefix of (":20" →
// ":200") and nothing else.
func TestEdgePrefixEqualsWholeKey(t *testing.T) {
	cfForModes(t, func(t *testing.T, m cfMode) {
		c := cfStart(t, m)
		p, q := uuid.New(), uuid.New()
		head := DomainPatientPrefix("condition", p)
		l20, l200, l10 := ConditionListKey(q, time.Time{}, uuid.Nil, 20), ConditionListKey(q, time.Time{}, uuid.Nil, 200), ConditionListKey(q, time.Time{}, uuid.Nil, 10)
		keys := []string{head, head + ":count", l20, l200, l10, DomainPatientPrefix("condition", q)}
		c.putAll(keys)
		if err := c.st[0].InvalidateDomain(context.Background(), "condition", p); err != nil {
			t.Fatal(err)
		}
		gone := map[string]bool{head: true, head + ":count": true}
		c.expectPresence(keys, gone, "sweep equal to a whole key")
		if err := c.st[2].R.DeletePrefix(context.Background(), l20); err != nil {
			t.Fatal(err)
		}
		gone[l20], gone[l200] = true, true
		c.expectPresence(keys, gone, "DeletePrefix of a whole key")
	})
}

// TestEdgeEmptyPrefixRefused: the consumer refuses an empty root or a nil profile with
// ErrEmptyPrefix and sends nothing — no key vanishes on any instance (a delete_prefix "" would
// clear the sheet). Sentinel keys sit exactly under the prefixes a bug WOULD send
// (":patient:<p>" for an empty root, "<root>:patient:00000000-…" for a nil profile), so a sweep
// that slipped through would remove them and the presence check would fail.
func TestEdgeEmptyPrefixRefused(t *testing.T) {
	cfForModes(t, func(t *testing.T, m cfMode) {
		c := cfStart(t, m)
		ctx := context.Background()
		p := uuid.New()
		keys, _ := cfCorpusKeys(uuid.New(), p)
		keys = append(keys,
			DomainPatientPrefix("", p)+":sentinel",
			DomainPatientPrefix("condition", uuid.Nil)+":sentinel",
		)
		c.putAll(keys)
		for _, err := range []error{
			c.st[0].InvalidateDomain(ctx, "", p),
			c.st[1].InvalidateDomain(ctx, "condition", uuid.Nil),
			c.st[2].InvalidateDomainBoth(ctx, "", p, uuid.New()),
		} {
			if !errors.Is(err, ErrEmptyPrefix) {
				t.Fatalf("err = %v, want ErrEmptyPrefix", err)
			}
		}
		c.expectPresence(keys, nil, "refused empty sweeps") // the barrier orders any sent delete before it
	})
}

// TestEdgeUndecodableValueIsMiss: a cached value of another JSON shape does not decode into T, so
// CachedList treats it as a miss, the loader runs, and its value replaces the bad one everywhere.
func TestEdgeUndecodableValueIsMiss(t *testing.T) {
	cfForModes(t, func(t *testing.T, m cfMode) {
		c := cfStart(t, m)
		p := uuid.New()
		c.db.Put(p, Row{Profile: p, Value: 6})
		key := ConditionListKey(p, time.Time{}, uuid.Nil, 20)
		if err := c.st[0].R.SetWithTTL(context.Background(), key, map[string]string{"not": "a row list"}, TTLVitals); err != nil {
			t.Fatal(err)
		}
		c.barrier()
		if !c.st[1].R.Has(key) {
			t.Fatal("the foreign-shape value was not cached")
		}
		if v := value(c.readList(c.st[1], key, p, TTLVitals)); v != 6 {
			t.Fatalf("read = %d, want 6 from the loader", v)
		}
		c.expectLoads(1, "an undecodable value is a miss")
		c.seenEverywhere(key, 6)
		c.readList(c.st[2], key, p, TTLVitals)
		c.expectLoads(1, "the loaded value replaced it")
	})
}

// TestEdgeZeroValueCached: a load that returns the zero value (0, an empty list) is cached like any
// other value: the load count stays at one.
func TestEdgeZeroValueCached(t *testing.T) {
	cfForModes(t, func(t *testing.T, m cfMode) {
		c := cfStart(t, m)
		ctx := context.Background()
		p := uuid.New() // no rows: Load returns a nil slice
		countKey := DomainPatientPrefix("condition", p) + ":count"
		listKey := ConditionListKey(p, time.Time{}, uuid.Nil, 20)
		count := func() (int, error) { rows, err := c.db.Load(p); return len(rows), err }

		if n, err := CachedOne(c.st[0], ctx, countKey, TTLVitals, count); err != nil || n != 0 {
			t.Fatalf("count = %d, %v", n, err)
		}
		if rows := c.readList(c.st[0], listKey, p, TTLVitals); len(rows) != 0 {
			t.Fatalf("list = %v", rows)
		}
		c.expectLoads(2, "one load each")
		c.barrier()
		for i, st := range c.st {
			var n int
			var rows []Row
			if !st.R.Get(countKey, &n) || n != 0 {
				t.Fatalf("instance %d: zero count not cached", i)
			}
			if !st.R.Get(listKey, &rows) || len(rows) != 0 {
				t.Fatalf("instance %d: empty list not cached", i)
			}
			CachedOne(st, ctx, countKey, TTLVitals, count)
			c.readList(st, listKey, p, TTLVitals)
		}
		c.expectLoads(2, "zero values are hits")
	})
}
