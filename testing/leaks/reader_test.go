// Covers: memory and goroutine bounds

package leaks

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/CrimsonCoder42/red_lite_cache/protocol"
	"github.com/CrimsonCoder42/red_lite_cache/reader"
	"github.com/CrimsonCoder42/red_lite_cache/testing/integration"
)

// Reader layer (D-038, agent C): the local map bound, the miss-record bound, the pending-delete
// bound, expiry reaching readers, and a close racing an expiry batch. No reader clock is needed
// (D-039): expiry reaches a reader as invalidate{keys}; L3/L4 use no TTL.

// readerPipeline is how many sets the raw writer sends before it reads their replies: well under
// the server's per-conn send queue (ConnSendQueue 256), so the writer is never kicked.
const readerPipeline = 200

// readerWriteBatch sends keys as sets from the raw conn w, pipelined readerPipeline at a time, and
// fails on any refused set.
func readerWriteBatch(t *testing.T, w *integration.RawConn, keys []string, id *uint64) {
	t.Helper()
	for start := 0; start < len(keys); start += readerPipeline {
		end := min(start+readerPipeline, len(keys))
		for _, k := range keys[start:end] {
			*id++
			w.Send(protocol.Request{Action: protocol.ActionSet, ID: *id, Key: k, Value: json.RawMessage(`"v"`)})
		}
		for range end - start {
			if resp, _ := w.Reply(); !resp.Success {
				t.Fatalf("raw set refused: %+v", resp)
			}
		}
	}
}

// readerHeapGrowthPct is the L3 tolerance: HeapAlloc at sample 50 may exceed sample 10 by at most
// 10% (the request's number) — the whole process, so GC noise is inside it.
const readerHeapGrowthPct = 10

// L3: a reader with MaxEntries 1 000 receives 50 000 distinct replicate frames; its map never exceeds
// the bound, its miss records stay under missSeqMax, and the heap does not grow with the stream.
// The writer deletes batch b-2 after batch b is sampled so the SERVER's store stays ~3 000 keys
// (the server is unbounded by default and shares the heap); the reader still evicts on every batch.
func TestL3ReaderBoundHoldsUnderChurn(t *testing.T) {
	const (
		maxEntries = 1_000
		batches    = 50
		perBatch   = 1_000
		missMax    = 65_536 // reader.missSeqMax
	)
	s := NewSteps(t)
	srv := integration.StartLeader(t)
	sheet := integration.UniqueSheet(t)
	rd := integration.NewReader(t, srv, sheet, func(c *reader.Config) { c.MaxEntries = maxEntries })
	w, _ := integration.OpenRaw(t, srv, sheet)
	g0 := integration.GoroutineBaseline(t)
	base := rd.Stats()
	s.Baseline("goroutines %d, reader %+v", g0, base)

	s.Fill("%d batches × %d distinct keys through a raw conn → replicate frames to the reader", batches, perBatch)
	var id uint64
	peak := 0
	var heap10, heap50 uint64
	for b := range batches {
		keys := make([]string, perBatch)
		for i := range keys {
			keys[i] = fmt.Sprintf("b%02d:k%04d", b, i)
		}
		readerWriteBatch(t, w, keys, &id)
		// The last key is the last replicate of the batch: once it is held, every frame of the batch
		// was applied (one read loop, server order), and nothing after it can evict it.
		last := keys[len(keys)-1]
		integration.Eventually(t, "reader holds "+last, func() bool { return rd.Has(last) })
		st := rd.Stats()
		peak = max(peak, st.Entries)
		if st.Entries > maxEntries {
			t.Fatalf("sample %d: Entries %d > MaxEntries %d", b+1, st.Entries, maxEntries)
		}
		if st.MissRecords > missMax {
			t.Fatalf("sample %d: MissRecords %d > %d", b+1, st.MissRecords, missMax)
		}
		switch b + 1 {
		case 10:
			heap10 = integration.HeapAlloc()
		case 50:
			heap50 = integration.HeapAlloc()
		}
		if b >= 2 {
			if resp := w.Do(protocol.Request{Action: protocol.ActionDeletePrefix, ID: id + 1, Prefix: fmt.Sprintf("b%02d:", b-2)}); !resp.Success {
				t.Fatalf("delete_prefix batch %d: %+v", b-2, resp)
			}
			id++
		}
	}
	st := rd.Stats()
	s.Observe("peak Entries %d (bound %d), final %+v, heap@10 %d heap@50 %d, server %+v", peak, maxEntries, st, heap10, heap50, srv.App.Registry().Stats().Store)
	Above(t, "reader Entries", peak, base.Entries)
	if peak != maxEntries {
		t.Errorf("peak Entries %d, want the bound %d reached (eviction exercised)", peak, maxEntries)
	}
	if limit := heap10 + heap10*readerHeapGrowthPct/100; heap50 > limit {
		t.Errorf("HeapAlloc grew %d → %d (> %d%%)", heap10, heap50, readerHeapGrowthPct)
	}

	s.Drain("DeletePrefix \"\" from the reader")
	if err := rd.DeletePrefix(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	integration.Eventually(t, "reader map empty", func() bool { return rd.Stats().Entries == 0 })
	g := integration.WaitGoroutines(t, g0, 0)
	s.Assert("reader %+v, goroutines %d (baseline %d), wall %s", rd.Stats(), g, g0, s.Elapsed())
	AtMost(t, "goroutines", g, g0, 0)
}

// L4: 20 000 deletes while disconnected: the queue never exceeds MaxPendingDeletes (10 000) and
// collapses to one delete_prefix "" (D-005); reconnect replays it and the queue is empty.
func TestL4PendingDeletesBound(t *testing.T) {
	const (
		deletes = 20_000
		maxQ    = reader.DefaultMaxPendingDeletes
	)
	s := NewSteps(t)
	srv := integration.StartLeader(t)
	sheet := integration.UniqueSheet(t)
	p := integration.NewProxy(t, srv.Addr)
	rd := integration.NewReaderAt(t, "ws://"+p.Addr+protocol.PathWS, "", sheet)
	// "other" is never deleted by name: only a replayed delete_prefix "" removes it from the server.
	if err := rd.Set(context.Background(), "other", 1); err != nil {
		t.Fatal(err)
	}
	g0 := integration.GoroutineBaseline(t)
	base := rd.Stats()
	s.Baseline("goroutines %d, reader %+v", g0, base)

	s.Fill("block the proxy, %d Deletes of distinct keys", deletes)
	p.Block()
	integration.Eventually(t, "reader disconnected", func() bool { return !rd.IsConnected() })
	ctx := context.Background()
	for i := range maxQ {
		if err := rd.Delete(ctx, fmt.Sprintf("k%05d", i)); err != nil {
			t.Fatal(err)
		}
	}
	atCap := rd.Stats().PendingDeletes
	for i := maxQ; i < deletes; i++ {
		if err := rd.Delete(ctx, fmt.Sprintf("k%05d", i)); err != nil {
			t.Fatal(err)
		}
		if n := rd.Stats().PendingDeletes; n > maxQ {
			t.Fatalf("delete %d: PendingDeletes %d > %d", i, n, maxQ)
		}
	}
	after := rd.Stats().PendingDeletes
	s.Observe("PendingDeletes %d at %d deletes (cap %d), %d after %d", atCap, maxQ, maxQ, after, deletes)
	Above(t, "PendingDeletes", atCap, base.PendingDeletes)
	Within(t, "PendingDeletes at the cap", atCap, maxQ, 0)
	Within(t, "PendingDeletes after overflow (one delete_prefix \"\")", after, 1, 0)

	s.Drain("unblock the proxy: reconnect replays the queue")
	p.Unblock()
	integration.Eventually(t, "reconnected with an empty queue", func() bool {
		return rd.IsConnected() && rd.Stats().PendingDeletes == 0
	})
	g := integration.WaitGoroutines(t, g0, 0)
	_, st := integration.OpenRaw(t, srv, sheet)
	s.Assert("reader %+v, server keys %d, goroutines %d (baseline %d)", rd.Stats(), len(st.Data), g, g0)
	if _, ok := st.Data["other"]; ok {
		t.Error(`"other" survived: the replay was not one delete_prefix ""`)
	}
	AtMost(t, "goroutines", g, g0, 0)
}

// L2 (reader half): 5 000 keys with TTL 1 minute reach 3 readers; the clock moves 2 minutes and ONE
// expiry pass empties every reader through invalidate{keys}.
func TestL2ReaderEntriesFreedByExpire(t *testing.T) {
	const keys = 5_000
	s := NewSteps(t)
	c := integration.NewClock()
	srv := integration.StartLeader(t, integration.WithClock(c))
	sheet := integration.UniqueSheet(t)
	rs := []*reader.Reader{
		integration.NewReader(t, srv, sheet),
		integration.NewReader(t, srv, sheet),
		integration.NewReader(t, srv, sheet),
	}
	g0 := integration.GoroutineBaseline(t)
	s.Baseline("goroutines %d, readers %+v %+v %+v", g0, rs[0].Stats(), rs[1].Stats(), rs[2].Stats())

	s.Fill("%d SetWithTTL(ttl=1) through reader 0, 8 writers", keys)
	readerSetAll(t, rs[0], "k", keys, 1)
	for i, r := range rs {
		integration.Eventually(t, fmt.Sprintf("reader %d holds %d", i, keys), func() bool { return r.Stats().Entries == keys })
	}
	peak := rs[2].Stats().Entries
	s.Observe("every reader Entries %d, server %+v", peak, srv.App.Registry().Stats().Store)
	Above(t, "reader Entries", peak, 0)

	s.Drain("clock +2m, one ExpireAll pass")
	c.Advance(2 * time.Minute)
	n := srv.App.Registry().ExpireAll(c.Now()) // the janitor may have taken some first; same pass shape
	if left := srv.App.Registry().Stats().Store.Entries; left != 0 {
		t.Fatalf("server still holds %d keys after the pass (this call expired %d)", left, n)
	}
	for i, r := range rs {
		integration.Eventually(t, fmt.Sprintf("reader %d empty", i), func() bool { return r.Stats().Entries == 0 })
	}
	g := integration.WaitGoroutines(t, g0, 0)
	s.Assert("expired by this call %d, readers %+v %+v %+v, goroutines %d (baseline %d), wall %s",
		n, rs[0].Stats(), rs[1].Stats(), rs[2].Stats(), g, g0, s.Elapsed())
	AtMost(t, "goroutines", g, g0, 0)
}

// readerSetAll writes n keys prefix0..prefix(n-1) with ttl minutes through r, 8 at a time.
func readerSetAll(t *testing.T, r *reader.Reader, prefix string, n, ttl int) {
	t.Helper()
	const workers = 8
	var wg sync.WaitGroup
	for w := range workers {
		wg.Go(func() {
			for i := w; i < n; i += workers {
				if err := r.SetWithTTL(context.Background(), fmt.Sprintf("%s%05d", prefix, i), i, ttl); err != nil {
					t.Errorf("set %s%05d: %v", prefix, i, err)
					return
				}
			}
		})
	}
	wg.Wait()
	if t.Failed() {
		t.FailNow()
	}
}

// Edge: a reader closed while an expiry batch (invalidate{keys}) is on its way: Close returns, no
// panic, its goroutines exit, and it holds nothing (Stats zero — the lingering-data fix).
func TestEdgeReaderClosedDuringExpireBatch(t *testing.T) {
	const (
		rounds = 3
		keys   = 2_000
	)
	s := NewSteps(t)
	c := integration.NewClock()
	srv := integration.StartLeader(t, integration.WithClock(c))
	sheet := integration.UniqueSheet(t)
	writer := integration.NewReader(t, srv, sheet)
	for round := range rounds {
		g0 := integration.GoroutineBaseline(t)
		s.Baseline("round %d: goroutines %d", round, g0)
		victim := integration.NewReader(t, srv, sheet)
		prefix := fmt.Sprintf("r%d:", round)
		readerSetAll(t, writer, prefix, keys, 1)
		integration.Eventually(t, "victim holds the round's keys", func() bool { return victim.Stats().Entries >= keys })
		st := victim.Stats()
		s.Fill("round %d: victim %+v", round, st)
		s.Observe("round %d: victim Entries %d", round, st.Entries)
		Above(t, "victim Entries", st.Entries, 0)

		s.Drain("round %d: clock +2m, ExpireAll and victim.Close() at once", round)
		c.Advance(2 * time.Minute)
		done := make(chan int, 1)
		go func() { done <- srv.App.Registry().ExpireAll(c.Now()) }()
		if err := victim.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		n := <-done
		g := integration.WaitGoroutines(t, g0, 0)
		got := victim.Stats()
		s.Assert("round %d: expired %d, victim after Close %+v, goroutines %d (baseline %d)", round, n, got, g, g0)
		if got != (reader.Stats{}) {
			t.Errorf("round %d: closed reader holds %+v, want zero", round, got)
		}
		AtMost(t, "goroutines", g, g0, 0)
	}
}
