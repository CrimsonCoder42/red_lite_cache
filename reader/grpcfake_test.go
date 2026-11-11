// Covers: reader on the gRPC wire (D-048): chunked state, bearer metadata, broadcast order
package reader

import (
	"encoding/json"
	"errors"
	"fmt"
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
