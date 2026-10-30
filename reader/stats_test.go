// Covers: reader
package reader

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/CrimsonCoder42/red_lite_cache/protocol"
)

// manualClock is the D-039 test clock for newReader.
type manualClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *manualClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *manualClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func mustNewClock(t *testing.T, cfg Config, c *manualClock) *Reader {
	t.Helper()
	r, err := newReader(cfg, c.now)
	if err != nil {
		t.Fatalf("newReader: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func (r *Reader) waitSeq(t *testing.T, f *fakeServer, seq uint64) {
	t.Helper()
	f.do(func(f *fakeServer) { f.seq = seq })
	f.broadcast(protocol.Invalidate{Type: protocol.TypeInvalidate, Key: "zz", Seq: seq})
	waitFor(t, fmt.Sprintf("seq %d", seq), func() bool { return r.local.appliedSeq() == seq })
}

// D-039 finding: a key missed once and never set kept its miss record until missSeqMax keys
// accumulated. sweepMissLocked drops records with no live entry once per MissWindow; a record
// with a live entry is kept and still sends its seq (R6).
func TestMissRecordsSweptAfterWindow(t *testing.T) {
	f := newFakeServer(t)
	f.do(func(f *fakeServer) { f.seq = 5 })
	cfg := testConfig(f.addr())
	cfg.MissWindow = 10 * time.Second
	c := &manualClock{t: time.Now()}
	r := mustNewClock(t, cfg, c)

	const dead = 1000
	for i := range dead {
		if r.Has(fmt.Sprintf("dead%d", i)) { // record {seq 5, t0}
			t.Fatal("dead key should miss")
		}
	}
	if s := r.Stats(); s.MissRecords != dead {
		t.Fatalf("MissRecords = %d after %d misses, want %d", s.MissRecords, dead, dead)
	}

	c.advance(9 * time.Second)
	r.waitSeq(t, f, 9)
	if r.Has("live") { // record {seq 9, t0+9s}
		t.Fatal("live should miss")
	}
	c.advance(2 * time.Second) // t0+11s: the dead records are past the window, live is 2s old
	r.waitSeq(t, f, 12)
	if s := r.Stats(); s.MissRecords != dead+1 {
		t.Fatalf("MissRecords = %d before the sweep, want %d (nothing sweeps before a new-key miss)", s.MissRecords, dead+1)
	}
	if r.Has("new") { // a new-key miss past the window: sweep
		t.Fatal("new should miss")
	}
	if s := r.Stats(); s.MissRecords != 2 {
		t.Fatalf("MissRecords = %d after the sweep, want 2 (live, new)", s.MissRecords)
	}

	if err := r.Set(bg, "live", 1); err != nil { // the live record is kept: base_seq 9; ack seq 13
		t.Fatal(err)
	}
	if err := r.Set(bg, "dead0", 1); err != nil { // swept: no record, no fallback → applied seq 13
		t.Fatal(err)
	}
	sets := f.requests(protocol.ActionSet)
	for i, want := range []uint64{9, 13} {
		if sets[i].BaseSeq == nil || *sets[i].BaseSeq != want {
			t.Fatalf("set %s base_seq = %v, want %d", sets[i].Key, sets[i].BaseSeq, want)
		}
	}
}

// A sweep runs at most once per MissWindow: a dead record left by a miss right after a sweep stays
// until the next window, never longer.
func TestMissRecordsSweepOncePerWindow(t *testing.T) {
	f := newFakeServer(t)
	cfg := testConfig(f.addr())
	cfg.MissWindow = 10 * time.Second
	c := &manualClock{t: time.Now()}
	r := mustNewClock(t, cfg, c)
	r.Has("a") // t0
	c.advance(10 * time.Second)
	r.Has("b") // sweep at t0+10s: a dropped
	c.advance(10 * time.Second)
	r.Has("c") // sweep at t0+20s: b dropped
	if s := r.Stats(); s.MissRecords != 1 {
		t.Fatalf("MissRecords = %d, want 1 (c)", s.MissRecords)
	}
	c.advance(5 * time.Second)
	r.Has("d") // t0+25s: no sweep this window
	if s := r.Stats(); s.MissRecords != 2 {
		t.Fatalf("MissRecords = %d, want 2 (c, d)", s.MissRecords)
	}
}

// Lingering data: Close frees the local map, the miss records and the replay queue; Stats on a
// closed reader is zero, and reads after Close record no miss.
func TestStatsFreedOnClose(t *testing.T) {
	f := newFakeServer(t)
	f.put("k", `1`, 0)
	f.put("j", `2`, 0)
	r := mustNew(t, testConfig(f.addr()))
	r.Has("absent")
	f.setDown(true)
	waitFor(t, "disconnect", func() bool { return !r.IsConnected() })
	if err := r.Delete(bg, "x"); err != nil {
		t.Fatal(err)
	}
	if s := r.Stats(); s != (Stats{Entries: 2, PendingDeletes: 1, MissRecords: 1}) {
		t.Fatalf("Stats before Close = %+v, want {2 1 1}", s)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r.Has("after")
	if s := r.Stats(); s != (Stats{}) {
		t.Fatalf("Stats after Close = %+v, want zero", s)
	}
}

// Lingering data: a terminal close (D-030) frees the same, without the caller's Close.
func TestStatsFreedOnTerminalClose(t *testing.T) {
	f := newFakeServer(t)
	f.put("k", `1`, 0)
	r := mustNew(t, testConfig(f.addr()))
	r.Has("absent")
	if s := r.Stats(); s.Entries != 1 || s.MissRecords != 1 {
		t.Fatalf("Stats before the close = %+v", s)
	}
	f.closeAll(protocol.CloseKicked, protocol.CloseReasonKicked)
	waitFor(t, "terminal close", func() bool { return errors.Is(r.LastError(), ErrKicked) })
	waitFor(t, "released", func() bool { return r.Stats() == (Stats{}) })
}

// PendingDeletes counts a delete in flight on the connection (teardown would requeue it) as well as
// the replay queue.
func TestStatsPendingInFlight(t *testing.T) {
	f := newFakeServer(t)
	gate := make(chan struct{})
	f.do(func(f *fakeServer) { f.gate = gate })
	r := mustNew(t, testConfig(f.addr()))
	done := make(chan error, 1)
	go func() { done <- r.Delete(bg, "k") }()
	waitFor(t, "delete in flight", func() bool { return r.Stats().PendingDeletes == 1 })
	close(gate)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := r.Stats().PendingDeletes; n != 0 {
		t.Fatalf("PendingDeletes = %d after the ack, want 0", n)
	}
}
