// Covers: backendAdvo cache layer
package conformance

// C1, C2, C3, C10, C11: the read-through and the write-then-sweep, as backendAdvo's three
// instances do them (cache/shared.go:109-157, cache/store.go:269-275).

import (
	"context"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/CrimsonCoder42/red_lite_cache/testing/integration"
)

// TestC1ReadThroughCachesOnce: instance 1 misses and loads; instances 2 and 3 are served the value
// it cached; the database is hit once.
func TestC1ReadThroughCachesOnce(t *testing.T) {
	cfForModes(t, func(t *testing.T, m cfMode) {
		c := cfStart(t, m)
		p := uuid.New()
		c.db.Put(p, Row{ID: uuid.New(), Profile: p, Value: 1})
		s, e := win(0)
		key := BPReadingsPageKey(p, s, e, time.Time{}, uuid.Nil, 20)

		if v := value(c.readList(c.st[0], key, p, TTLVitals)); v != 1 {
			t.Fatalf("instance 1 read %d, want 1", v)
		}
		c.expectLoads(1, "instance 1's miss loads once")
		c.seenEverywhere(key, 1)
		for round := range 3 {
			for i, st := range c.st {
				if v := value(c.readList(st, key, p, TTLVitals)); v != 1 {
					t.Fatalf("round %d instance %d read %d, want 1", round, i, v)
				}
			}
		}
		c.expectLoads(1, "nine reads on three instances are all hits")
	})
}

// TestC2WriteInvalidatesEverywhere: a write through one instance and ONE InvalidateDomain (no
// second sweep) removes every view of that domain for that profile on all three instances; the
// next read on each loads the new row; no instance ever reads the old one.
func TestC2WriteInvalidatesEverywhere(t *testing.T) {
	cfForModes(t, func(t *testing.T, m cfMode) {
		c := cfStart(t, m)
		ctx := context.Background()
		p := uuid.New()
		c.db.Put(p, Row{ID: uuid.New(), Profile: p, Value: 1})
		s, e := win(1)
		keys := []string{ConditionListKey(p, time.Time{}, uuid.Nil, 20), DomainFeedWindowKey(DomainPatientPrefix("condition", p), s, e)}
		for _, k := range keys {
			c.readList(c.st[0], k, p, TTLVitals)
			c.seenEverywhere(k, 1)
		}
		for _, st := range c.st[1:] {
			for _, k := range keys {
				c.readList(st, k, p, TTLVitals)
			}
		}
		c.expectLoads(2, "one load per key, the rest hits")

		// The write, through instance 2: the database, then ONE sweep.
		c.db.Put(p, Row{ID: uuid.New(), Profile: p, Value: 2})
		if err := c.st[1].InvalidateDomain(ctx, "condition", p); err != nil {
			t.Fatalf("InvalidateDomain: %v", err)
		}
		c.barrier()
		for _, k := range keys {
			c.neverStale(k, 1, "after the sweep")
			for i, st := range c.st {
				if st.R.Has(k) {
					t.Fatalf("instance %d still holds %s after the sweep", i, k)
				}
			}
		}
		before := c.db.Loads()
		for round := range 2 {
			for i, st := range c.st {
				for _, k := range keys {
					if v := value(c.readList(st, k, p, TTLVitals)); v != 2 {
						t.Fatalf("round %d instance %d read %d from %s, want 2", round, i, v, k)
					}
				}
			}
			c.barrier()
			for _, k := range keys {
				c.neverStale(k, 1, "after the re-reads")
			}
			t.Logf("C2 round %d after the sweep: %d FakeDB loads for 6 reads", round, c.db.Loads()-before)
			before = c.db.Loads()
		}
		if got := c.db.Loads(); got < 4 || got > 2+12 {
			t.Fatalf("FakeDB loads = %d, want 4..14 (each key reloads at least once after the sweep)", got)
		}
	})
}

// TestC3ReadThroughRaceNeverRecaches: instance 2 is parked mid-load holding the OLD row; instance
// 1 (or 3) writes the database and sweeps; then instance 2's read-through Set lands. Nobody can
// read the old value afterwards — the set is refused (R6), not re-cached. 200 iterations, order
// forced with channels.
func TestC3ReadThroughRaceNeverRecaches(t *testing.T) {
	cfForModes(t, func(t *testing.T, m cfMode) {
		c := cfStart(t, m)
		ctx := context.Background()
		var park atomic.Pointer[uuid.UUID]
		parked := make(chan struct{})
		release := make(chan struct{})
		c.db.LoadHook = func(profile uuid.UUID) {
			if target := park.Load(); target != nil && *target == profile {
				parked <- struct{}{}
				<-release
			}
		}
		type result struct {
			rows []Row
			err  error
		}
		for i := range 200 {
			p := uuid.New()
			key := ConditionListKey(p, time.Time{}, uuid.Nil, 20)
			c.db.Put(p, Row{ID: uuid.New(), Profile: p, Value: 1})
			loads0 := c.db.Loads()

			park.Store(&p)
			done := make(chan result, 1)
			go func() {
				rows, err := CachedList(c.st[1], ctx, key, TTLVitals, c.loadRows(p))
				done <- result{rows, err}
			}()
			select {
			case <-parked: // instance 2 has read the OLD row and not yet returned it
			case <-time.After(integration.Timeout):
				t.Fatalf("iteration %d: instance 2 never reached the load", i)
			}
			park.Store(nil)

			writer := c.st[0]
			if i%2 == 1 {
				writer = c.st[2]
			}
			c.db.Put(p, Row{ID: uuid.New(), Profile: p, Value: 2})
			if err := writer.InvalidateDomain(ctx, "condition", p); err != nil {
				t.Fatalf("iteration %d: InvalidateDomain: %v", i, err)
			}
			release <- struct{}{}
			r := <-done
			if r.err != nil || value(r.rows) != 1 {
				t.Fatalf("iteration %d: the parked request returned %v, %v; want its own old row 1", i, r.rows, r.err)
			}

			c.barrier() // instance 2's Set was answered before done; its broadcast (if any) is applied now
			c.neverStale(key, 1, "iteration "+strconv.Itoa(i)+" after the racing set landed")
			for j, st := range c.st {
				if v := value(c.readList(st, key, p, TTLVitals)); v != 2 {
					t.Fatalf("iteration %d: instance %d read %d after the race, want 2", i, j, v)
				}
			}
			c.barrier()
			c.neverStale(key, 1, "iteration "+strconv.Itoa(i)+" after the re-reads")
			if got := c.db.Loads() - loads0; got < 2 || got > 4 {
				t.Fatalf("iteration %d: %d loads, want 2..4 (the parked one + 1..3 reloads)", i, got)
			}
		}
	})
}

// TestC10ErrorsNeverCached: a failing load returns its error to the caller and caches nothing on
// any instance; the next read after recovery loads and caches.
func TestC10ErrorsNeverCached(t *testing.T) {
	cfForModes(t, func(t *testing.T, m cfMode) {
		c := cfStart(t, m)
		p := uuid.New()
		c.db.Put(p, Row{ID: uuid.New(), Profile: p, Value: 7})
		key := ConditionListKey(p, time.Time{}, uuid.Nil, 20)

		c.db.Fail(true)
		for i, st := range c.st[:2] {
			if _, err := CachedList(st, context.Background(), key, TTLVitals, c.loadRows(p)); !errors.Is(err, ErrDB) {
				t.Fatalf("instance %d: err = %v, want ErrDB", i, err)
			}
		}
		c.expectLoads(2, "two failing loads")
		c.barrier()
		for i, st := range c.st {
			if st.R.Has(key) {
				t.Fatalf("instance %d holds %s after a failed load", i, key)
			}
		}

		c.db.Fail(false)
		if v := value(c.readList(c.st[0], key, p, TTLVitals)); v != 7 {
			t.Fatalf("read after recovery = %d, want 7", v)
		}
		c.expectLoads(3, "the first read after recovery loads")
		c.seenEverywhere(key, 7)
		for _, st := range c.st {
			c.readList(st, key, p, TTLVitals)
		}
		c.expectLoads(3, "then it is cached for all three")
	})
}

// decodeCursor is the API side of EncodeCursor: base64url "<nanos>:<uuid>" → the pair.
func decodeCursor(t *testing.T, s string) (time.Time, uuid.UUID) {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("cursor %q: %v", s, err)
	}
	ns, id, ok := strings.Cut(string(b), ":")
	if !ok {
		t.Fatalf("cursor %q: no ':'", b)
	}
	n, err := strconv.ParseInt(ns, 10, 64)
	if err != nil {
		t.Fatalf("cursor %q: %v", b, err)
	}
	return time.Unix(0, n), uuid.MustParse(id)
}

// TestC11CursorKeysDistinct: page one and page two of one list are two keys with two values;
// one sweep clears both on every instance.
func TestC11CursorKeysDistinct(t *testing.T) {
	cfForModes(t, func(t *testing.T, m cfMode) {
		c := cfStart(t, m)
		p, pageTwoRows := uuid.New(), uuid.New()
		c.db.Put(p, Row{Profile: p, Value: 1})
		c.db.Put(pageTwoRows, Row{Profile: p, Value: 2})

		ct, cid := time.Date(2026, 10, 2, 8, 0, 0, 123456789, time.UTC), uuid.New()
		dt, did := decodeCursor(t, EncodeCursor(ct, cid))
		page1 := ConditionListKey(p, time.Time{}, uuid.Nil, 2)
		page2 := ConditionListKey(p, dt, did, 2)
		if page1 == page2 || page2 != ConditionListKey(p, ct, cid, 2) {
			t.Fatalf("cursor keys: page1 %q page2 %q", page1, page2)
		}
		if !strings.HasSuffix(page1, ":list:0:none:2") {
			t.Fatalf("page one key %q does not render the absent cursor as 0:none", page1)
		}

		c.readList(c.st[0], page1, p, TTLVitals)
		c.readList(c.st[0], page2, pageTwoRows, TTLVitals)
		c.expectLoads(2, "one load per page")
		c.seenEverywhere(page1, 1)
		c.seenEverywhere(page2, 2)
		for _, st := range c.st {
			if value(c.readList(st, page1, p, TTLVitals)) != 1 || value(c.readList(st, page2, pageTwoRows, TTLVitals)) != 2 {
				t.Fatal("a page was served the other page's rows")
			}
		}
		c.expectLoads(2, "both pages cached separately")

		if err := c.st[2].InvalidateDomain(context.Background(), "condition", p); err != nil {
			t.Fatalf("InvalidateDomain: %v", err)
		}
		c.expectPresence([]string{page1, page2}, map[string]bool{page1: true, page2: true}, "one sweep")
	})
}
