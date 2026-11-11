// Covers: reader panic counter (S1, doc/reader_optimize_2026-11-10)
package reader

import (
	"context"
	"testing"
)

// TestPanicCounted: a panic inside a session's read loop is recovered (the link resets) and counted
// in Stats().Panics, and the count survives Close so a test can check it at cleanup. This test
// panics on purpose: a session with no socket makes the read loop's first read panic.
func TestPanicCounted(t *testing.T) {
	f := newFakeServer(t)
	r := mustNew(t, testConfig(f.addr()))
	if n := r.Stats().Panics; n != 0 {
		t.Fatalf("Panics before = %d", n)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.readLoop(&session{addr: "nil-conn", ctx: ctx, cancel: cancel, pending: map[uint64]*call{}})
	if n := r.Stats().Panics; n != 1 {
		t.Fatalf("Panics after one recovered panic = %d, want 1", n)
	}
	waitFor(t, "still connected (the panicking session was not current)", r.IsConnected)
	r.Close()
	if n := r.Stats().Panics; n != 1 {
		t.Fatalf("Panics after Close = %d, want 1 (kept for the cleanup check)", n)
	}
}
