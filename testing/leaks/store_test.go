// Covers: memory and goroutine bounds
package leaks

// Agent A layer: the server store (L1), the janitor's expiry broadcast (L2, server half), the sweep
// log bound (L5) and lock cleanup (L9), plus the store/janitor edge cases. Each test walks the five
// D-038 steps through Steps.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/CrimsonCoder42/red_lite_cache/internal/cache"
	"github.com/CrimsonCoder42/red_lite_cache/internal/lock"
	"github.com/CrimsonCoder42/red_lite_cache/internal/sheet"
	"github.com/CrimsonCoder42/red_lite_cache/protocol"
	"github.com/CrimsonCoder42/red_lite_cache/reader"
	"github.com/CrimsonCoder42/red_lite_cache/testing/conformance"
	"github.com/CrimsonCoder42/red_lite_cache/testing/integration"
)

// storeKey is one generated key and the profile head it sits under ("" = not under a head).
type storeKey struct {
	key  string
	root string
	prof uuid.UUID
}

// storeRoots returns the first n distinct roots of conformance.Corpus that have profile-scoped shapes.
func storeRoots(t testing.TB, n int) []string {
	t.Helper()
	seen := map[string]bool{}
	var roots []string
	for _, s := range conformance.Corpus {
		if s.ProfileScoped && !seen[s.Root] {
			seen[s.Root] = true
			roots = append(roots, s.Root)
			if len(roots) == n {
				return roots
			}
		}
	}
	t.Fatalf("corpus has %d profile-scoped roots, want %d", len(roots), n)
	return nil
}

// storeKeys builds profiles × roots × perHead distinct corpus keys: for each (profile, root) it walks
// seeds over every profile-scoped shape of that root until perHead new keys exist.
func storeKeys(t testing.TB, profiles, roots, perHead int) []storeKey {
	t.Helper()
	tenant := uuid.NewSHA1(uuid.NameSpaceOID, []byte("leaks-tenant"))
	rs := storeRoots(t, roots)
	seen := map[string]bool{}
	out := make([]storeKey, 0, profiles*roots*perHead)
	for p := range profiles {
		prof := uuid.NewSHA1(uuid.NameSpaceOID, fmt.Appendf(nil, "leaks-profile-%d", p))
		for _, root := range rs {
			got := 0
			for seed := 0; got < perHead && seed < 10_000; seed++ {
				for _, s := range conformance.Corpus {
					if got == perHead {
						break
					}
					if s.Root != root || !s.ProfileScoped {
						continue
					}
					k := s.Sample(prof, tenant, seed)
					if seen[k] {
						continue
					}
					seen[k] = true
					got++
					out = append(out, storeKey{key: k, root: root, prof: prof})
				}
			}
			if got < perHead {
				t.Fatalf("root %s: only %d distinct keys, want %d", root, got, perHead)
			}
		}
	}
	return out
}

// storeZero fails unless every count of st is 0.
func storeZero(t testing.TB, what string, st cache.Stats) {
	t.Helper()
	if st != (cache.Stats{}) {
		t.Errorf("ASSERT %s: stats %+v, want all zero", what, st)
	}
}

func storeFill(s *cache.Store, keys []storeKey, exp time.Time) {
	v := json.RawMessage(`{"v":1}`)
	for _, k := range keys {
		s.Set(k.key, cache.Entry{Value: v, ExpiresAt: exp})
	}
}

// TestL1StoreExpireFreesEntriesAndIndex: 10 000 corpus keys (50 profiles × 8 roots × 25) drained
// three ways — Expire, DeletePrefix per profile head, Replace(empty) — each back to all-zero Stats.
func TestL1StoreExpireFreesEntriesAndIndex(t *testing.T) {
	st := NewSteps(t)
	keys := storeKeys(t, 50, 8, 25)
	if len(keys) != 10_000 {
		t.Fatalf("generated %d keys, want 10000", len(keys))
	}
	// every corpus key of a profile-scoped shape must sit under its profile head, or the
	// DeletePrefix drain below would miss it
	var offHead []string
	for _, k := range keys {
		if !strings.HasPrefix(k.key, conformance.DomainPatientPrefix(k.root, k.prof)) {
			offHead = append(offHead, k.key)
		}
	}
	t.Logf("keys not under a profile head: %d", len(offHead))

	s := cache.New()
	now := time.Now()
	heap0 := integration.HeapAlloc()
	base := s.Stats()
	st.Baseline("store stats %+v heap %d", base, heap0)
	storeZero(t, "baseline", base)

	// 1. Expire
	storeFill(s, keys, now.Add(time.Minute))
	st.Fill("10000 keys ttl 1m")
	peak := s.Stats()
	st.Observe("stats %+v heap %d", peak, integration.HeapAlloc())
	Above(t, "entries", peak.Entries, base.Entries)
	Above(t, "head buckets", peak.HeadBuckets, base.HeadBuckets)
	gone := s.Expire(now.Add(2 * time.Minute))
	st.Drain("Expire(now+2m) removed %d", len(gone))
	if len(gone) != len(keys) {
		t.Errorf("Expire removed %d, want %d", len(gone), len(keys))
	}
	st.Assert("after Expire %+v", s.Stats())
	storeZero(t, "after Expire", s.Stats())

	// 2. DeletePrefix per (root, profile) head
	storeFill(s, keys, time.Time{})
	peak = s.Stats()
	st.Fill("10000 keys no ttl: %+v", peak)
	Above(t, "entries (no ttl)", peak.Entries, 0)
	heads := map[string]bool{}
	for _, k := range keys {
		heads[conformance.DomainPatientPrefix(k.root, k.prof)] = true
	}
	n := 0
	for h := range heads {
		n += s.DeletePrefix(h)
	}
	for _, k := range offHead {
		if s.Delete(k) {
			n++
		}
	}
	st.Drain("DeletePrefix over %d heads + %d exact deletes removed %d", len(heads), len(offHead), n)
	if n != len(keys) {
		t.Errorf("DeletePrefix removed %d, want %d", n, len(keys))
	}
	st.Assert("after DeletePrefix %+v", s.Stats())
	storeZero(t, "after DeletePrefix", s.Stats())

	// 3. Replace(empty)
	storeFill(s, keys, time.Time{})
	peak = s.Stats()
	st.Fill("10000 keys again: %+v", peak)
	Above(t, "entries (replace)", peak.Entries, 0)
	s.Replace(map[string]cache.Entry{})
	st.Drain("Replace(empty)")
	st.Assert("after Replace %+v heap %d (was %d)", s.Stats(), integration.HeapAlloc(), heap0)
	storeZero(t, "after Replace", s.Stats())

	t.Logf("L1 wall %s", st.Elapsed())
	if st.Elapsed() > 10*time.Second {
		t.Logf("FINDING: L1 wall %s > 10s", st.Elapsed())
	}
}

// TestL2JanitorBroadcastsAndFrees (server half): 5 000 keys with ttl 1 min through 3 readers, the
// clock moved 2 min, ONE ExpireAll pass → the server store is all zero and no reader Has any key.
func TestL2JanitorBroadcastsAndFrees(t *testing.T) {
	st := NewSteps(t)
	clock := integration.NewClock()
	srv := integration.StartLeader(t, integration.WithClock(clock))
	name := integration.UniqueSheet(t)
	rs := []*reader.Reader{
		integration.NewReader(t, srv, name), integration.NewReader(t, srv, name), integration.NewReader(t, srv, name),
	}
	integration.Eventually(t, "3 readers connected", func() bool {
		for _, r := range rs {
			if !r.IsConnected() {
				return false
			}
		}
		return true
	})
	reg := srv.App.Registry()
	base := reg.Stats()
	st.Baseline("registry %+v", base)
	storeZero(t, "baseline store", base.Store)

	keys := storeKeys(t, 25, 8, 25) // 5 000
	// 4 writers per reader, each a contiguous slice of the keys: the fill is ack-bound, so a
	// sequential loop would spend the 10 s budget on round trips
	const writers = 12
	var wg sync.WaitGroup
	chunk := (len(keys) + writers - 1) / writers
	for w := range writers {
		lo, hi := w*chunk, min((w+1)*chunk, len(keys))
		r := rs[w%len(rs)]
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(context.Background(), integration.Timeout)
			defer cancel()
			for _, k := range keys[lo:hi] {
				if err := r.SetWithTTL(ctx, k.key, 1, 1); err != nil {
					t.Errorf("set %s: %v", k.key, err)
					return
				}
			}
		})
	}
	wg.Wait()
	if t.Failed() {
		t.FailNow()
	}
	st.Fill("%d keys ttl 1m through %d writers", len(keys), writers)
	integration.Eventually(t, "every reader has every key", func() bool {
		for _, r := range rs {
			for _, k := range keys {
				if !r.Has(k.key) {
					return false
				}
			}
		}
		return true
	})
	peak := reg.Stats()
	st.Observe("registry %+v", peak)
	Above(t, "server entries", peak.Store.Entries, base.Store.Entries)

	clock.Advance(2 * time.Minute)
	n := reg.ExpireAll(clock.Now())
	st.Drain("clock +2m, one ExpireAll pass expired %d (the janitor tick may take the rest)", n)
	integration.Eventually(t, "server store empty and no reader has any key", func() bool {
		if reg.Stats().Store != (cache.Stats{}) {
			return false
		}
		for _, r := range rs {
			for _, k := range keys {
				if r.Has(k.key) {
					return false
				}
			}
		}
		return true
	})
	final := reg.Stats()
	st.Assert("registry %+v", final)
	storeZero(t, "server store", final.Store)
	t.Logf("L2 wall %s", st.Elapsed())
	if st.Elapsed() > 10*time.Second {
		t.Logf("FINDING: L2 wall %s > 10s", st.Elapsed())
	}
}

// TestL5SweepLogBound: 50 000 writes never hold more than SweepLogMax log entries; once the window
// has passed, the next write prunes every older entry.
func TestL5SweepLogBound(t *testing.T) {
	st := NewSteps(t)
	clock := integration.NewClock()
	const logMax, window = 4096, 30 * time.Second
	reg := sheet.NewRegistry(sheet.Options{SweepLogMax: logMax, SweepLogWindow: window, Now: clock.Now, BcryptCost: 4})
	sh, err := reg.Create(sheet.DefaultProject, "l5", "pw")
	if err != nil {
		t.Fatal(err)
	}
	base := reg.Stats().SweepLogEntries
	st.Baseline("sweep log %d", base)

	v := json.RawMessage(`1`)
	peak := 0
	for i := range 50_000 {
		if r := sh.Apply(nil, sheet.Mutation{Kind: sheet.OpSet, Key: fmt.Sprintf("k%d", i%1000), Value: v}, nil); r.Err != nil {
			t.Fatalf("write %d: %v", i, r.Err)
		}
		if (i+1)%1000 == 0 {
			n := reg.Stats().SweepLogEntries
			peak = max(peak, n)
			if n > logMax {
				t.Fatalf("after %d writes: sweep log %d > max %d", i+1, n, logMax)
			}
		}
	}
	st.Fill("50000 writes")
	st.Observe("sweep log peak %d (max %d)", peak, logMax)
	Above(t, "sweep log", peak, base)

	clock.Advance(window + time.Second)
	if r := sh.Apply(nil, sheet.Mutation{Kind: sheet.OpSet, Key: "after-window", Value: v}, nil); r.Err != nil {
		t.Fatal(r.Err)
	}
	st.Drain("clock + window + 1s, one write")
	got := reg.Stats().SweepLogEntries
	st.Assert("sweep log %d", got)
	Within(t, "sweep log after window", got, 1, 0)

	// idle sheet: no write, one janitor pass after the window empties the log (lead fix, D-038)
	clock.Advance(window + time.Second)
	n := reg.ExpireAll(clock.Now())
	st.Drain("idle: clock + window + 1s, one ExpireAll pass (expired %d keys)", n)
	got = reg.Stats().SweepLogEntries
	st.Assert("idle sweep log %d", got)
	Within(t, "sweep log after an idle janitor pass", got, 0, 0)
}

// TestEdgeStoreInactiveJanitorPrunes: on a replica (active() false) a janitor tick prunes the sweep
// logs ApplyReplicated filled and expires no key.
func TestEdgeStoreInactiveJanitorPrunes(t *testing.T) {
	st := NewSteps(t)
	clock := integration.NewClock()
	reg := sheet.NewRegistry(sheet.Options{SweepLogMax: 4096, SweepLogWindow: 30 * time.Second, Now: clock.Now})
	sh := reg.Restore(sheet.DefaultProject, "replica-sheet", []byte("hash"))
	st.Baseline("%+v", reg.Stats())
	for i := range 100 {
		sh.ApplyReplicated(sheet.Op{Kind: sheet.OpSet, Seq: uint64(i + 1), Key: fmt.Sprintf("k%d", i),
			Value: json.RawMessage(`1`), ExpiresAt: clock.Now().Add(time.Second)})
	}
	peak := reg.Stats()
	st.Fill("100 replicated sets")
	st.Observe("%+v", peak)
	Above(t, "sweep log", peak.SweepLogEntries, 0)

	clock.Advance(31 * time.Second) // past the window and past every key's expiry
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	var runErr error
	wg.Go(func() { runErr = sheet.RunJanitor(ctx, reg, 5*time.Millisecond, func() bool { return false }) })
	integration.Eventually(t, "inactive janitor prunes the sweep log", func() bool { return reg.Stats().SweepLogEntries == 0 })
	cancel()
	wg.Wait()
	final := reg.Stats()
	st.Drain("inactive ticks")
	st.Assert("%+v", final)
	if runErr != nil {
		t.Errorf("RunJanitor = %v", runErr)
	}
	Within(t, "keys kept (a replica never expires)", final.Store.Entries, 100, 0)
}

// TestL9LockCleanupFrees: 100 conns × 50 locks (lock_ttl 1 s); closing 50 conns frees theirs, the
// clock +2 s frees the rest, and closing every conn brings the goroutines back.
func TestL9LockCleanupFrees(t *testing.T) {
	st := NewSteps(t)
	clock := integration.NewClock()
	srv := integration.StartLeader(t, integration.WithClock(clock))
	locks := srv.App.Locks()
	g0 := integration.GoroutineBaseline(t)
	base := locks.Stats()
	st.Baseline("goroutines %d locks %+v", g0, base)

	const conns, perConn = 100, 50
	name := integration.UniqueSheet(t)
	cs := make([]*integration.RawConn, conns)
	for i := range cs {
		cs[i], _ = integration.OpenRaw(t, srv, name)
	}
	// pipelined: every conn sends its 50 locks, then every reply is read (50 frames < the 256-frame
	// send queue, so no conn is kicked for backpressure)
	for i, c := range cs {
		for j := range perConn {
			c.Send(protocol.Request{Action: protocol.ActionLock, Key: fmt.Sprintf("c%d-k%d", i, j), LockTTL: 1})
		}
	}
	for i, c := range cs {
		for j := range perConn {
			if r, _ := c.Reply(); !r.Success {
				t.Fatalf("conn %d lock %d: %+v", i, j, r)
			}
		}
	}
	st.Fill("%d conns × %d locks ttl 1s", conns, perConn)
	peak := locks.Stats()
	st.Observe("locks %+v goroutines %d", peak, integration.Goroutines(t))
	Above(t, "locks held", peak.Held, base.Held)
	Within(t, "locks held peak", peak.Held, conns*perConn, 0)
	Within(t, "owners peak", peak.Owners, conns, 0)

	for _, c := range cs[:conns/2] {
		c.Conn.Close(websocket.StatusNormalClosure, "bye")
	}
	integration.Eventually(t, "half the conns' locks released", func() bool {
		s := locks.Stats()
		return s.Held == conns*perConn/2 && s.Owners == conns/2
	})
	st.Drain("closed %d conns: locks %+v", conns/2, locks.Stats())

	clock.Advance(2 * time.Second)
	integration.Eventually(t, "lock cleaner frees the expired locks", func() bool {
		return locks.Stats() == (lock.Stats{})
	})
	st.Drain("clock +2s: locks %+v", locks.Stats())
	for _, c := range cs[conns/2:] {
		c.Conn.Close(websocket.StatusNormalClosure, "bye")
	}
	integration.Eventually(t, "no collaborators", func() bool { return srv.App.Registry().Stats().Collaborators == 0 })
	// tolerance 0: every conn is closed on both ends and the leader is the one from BASELINE
	g := integration.WaitGoroutines(t, g0, 0)
	final := locks.Stats()
	st.Assert("locks %+v goroutines %d (baseline %d)", final, g, g0)
	Within(t, "locks held", final.Held, 0, 0)
	Within(t, "owners", final.Owners, 0, 0)
	AtMost(t, "goroutines", g, g0, 0)
}

// TestEdgeStoreExpireEmpty: Expire on an empty store returns nil and leaves every count 0.
func TestEdgeStoreExpireEmpty(t *testing.T) {
	s := cache.New()
	if got := s.Expire(time.Now()); got != nil {
		t.Errorf("Expire on empty = %v, want nil", got)
	}
	storeZero(t, "empty after Expire", s.Stats())
}

// TestEdgeStoreDeletePrefixEmptyNoColon: DeletePrefix("") on a store of keys without ':' removes all.
func TestEdgeStoreDeletePrefixEmptyNoColon(t *testing.T) {
	st := NewSteps(t)
	s := cache.New()
	st.Baseline("%+v", s.Stats())
	for i := range 1000 {
		s.Set(fmt.Sprintf("plain%d", i), cache.Entry{Value: json.RawMessage(`1`)})
	}
	peak := s.Stats()
	st.Fill("1000 keys without ':'")
	st.Observe("%+v", peak)
	Above(t, "entries", peak.Entries, 0)
	n := s.DeletePrefix("")
	st.Drain("DeletePrefix(\"\") removed %d", n)
	Within(t, "removed", n, 1000, 0)
	st.Assert("%+v", s.Stats())
	storeZero(t, "after DeletePrefix(\"\")", s.Stats())
}

// TestEdgeStoreJanitorNoSheets: a janitor pass over an empty registry expires nothing, and
// RunJanitor with a cancelled ctx returns nil.
func TestEdgeStoreJanitorNoSheets(t *testing.T) {
	reg := sheet.NewRegistry(sheet.Options{})
	if n := reg.ExpireAll(time.Now()); n != 0 {
		t.Errorf("ExpireAll on empty registry = %d, want 0", n)
	}
	if st := reg.Stats(); st != (sheet.Stats{}) {
		t.Errorf("empty registry stats %+v, want zero", st)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sheet.RunJanitor(ctx, reg, time.Millisecond, nil); err != nil {
		t.Errorf("RunJanitor(cancelled) = %v, want nil", err)
	}
}
