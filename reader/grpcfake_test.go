// Covers: reader on the gRPC wire (D-048): chunked state, bearer metadata, broadcast order
package reader

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/CrimsonCoder42/red_lite_cache/protocol"
	"github.com/CrimsonCoder42/red_lite_cache/protocol/rpcwire"
)

// A sheet state far larger than one chunk is assembled from its chunks and bulk-built.
func TestGRPCOpenChunkedState(t *testing.T) {
	f := newFakeServer(t)
	const n = 5000
	for i := range n {
		f.put(fmt.Sprintf("k%05d", i), fmt.Sprintf(`{"i":%d}`, i), 0)
	}
	r := mustNew(t, testConfig(f.addr()))
	for i := range n {
		var v struct{ I int }
		if !r.Get(fmt.Sprintf("k%05d", i), &v) || v.I != i {
			t.Fatalf("k%05d = %+v after a chunked open", i, v)
		}
	}
	if r.Has("missing") {
		t.Fatal("a key the state did not carry")
	}
}

// Replicate and invalidate broadcasts apply in the order sent.
func TestGRPCBroadcastOrder(t *testing.T) {
	f := newFakeServer(t)
	r := mustNew(t, testConfig(f.addr()))
	f.broadcast(protocol.Replicate{Type: protocol.TypeReplicate, Key: "b", Value: json.RawMessage(`2`), Seq: 10})
	f.broadcast(protocol.Invalidate{Type: protocol.TypeInvalidate, Key: "b", Seq: 11})
	f.broadcast(protocol.Replicate{Type: protocol.TypeReplicate, Key: "c", Value: json.RawMessage(`3`), Seq: 12})
	waitFor(t, "c replicated", func() bool { return r.Has("c") })
	if r.Has("b") {
		t.Fatal("invalidate applied out of order")
	}
}

// A refused open (UNAUTHENTICATED) is ErrAuthFailed from New; the API key rides as the bearer of
// every stream, never in the message.
func TestGRPCOpenRefusedAndAPIKey(t *testing.T) {
	f := newFakeServer(t)
	f.do(func(f *fakeServer) {
		f.openErr, f.openCode = protocol.ErrMsgNotAuthorized, rpcwire.ClosePolicyViolation
	})
	cfg := testConfig(f.addr())
	cfg.APIKey = "rlk_test_key"
	if r, err := New(cfg); r != nil || !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("New = %v, %v; want ErrAuthFailed", r, err)
	}
	f.do(func(f *fakeServer) { f.openErr = "" })
	mustNew(t, cfg)
	f.do(func(f *fakeServer) {
		for _, k := range f.keys {
			if k != cfg.APIKey {
				t.Fatalf("stream bearer %q, want the API key", k)
			}
		}
	})
}

// A value over one chunk (1.1 MiB) arrives alone in its own state chunk and as a broadcast, under
// the reader's receive limit; a set over the message limit is refused before the wire.
func TestGRPCValueOverChunk(t *testing.T) {
	f := newFakeServer(t)
	f.do(func(f *fakeServer) { f.chunk = rpcwire.DefaultChunkBytes })
	big := `"` + strings.Repeat("x", 1_153_434) + `"`
	f.put("a", `1`, 0)
	f.put("big", big, 0)
	f.put("c", `3`, 0)
	r := mustNew(t, testConfig(f.addr()))
	var got string
	if !r.Get("big", &got) || len(got) != 1_153_434 || !r.Has("a") || !r.Has("c") {
		t.Fatalf("state: big %d bytes, a %v, c %v", len(got), r.Has("a"), r.Has("c"))
	}
	f.broadcast(protocol.Replicate{Type: protocol.TypeReplicate, Key: "big2", Value: json.RawMessage(big), Seq: 5})
	waitFor(t, "big2 replicated", func() bool { return r.Has("big2") })
	err := r.Set(context.Background(), "huge", strings.Repeat("x", rpcwire.DefaultMaxMessageBytes))
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("set over the limit = %v, want ErrTooLarge", err)
	}
	if n := len(f.requests(protocol.ActionSet)); n != 0 {
		t.Fatalf("%d sets reached the wire", n)
	}
}
