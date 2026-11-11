// Covers: reader R2 / D-005 end to end (pending-delete overflow → delete_prefix "" on the real server)
package integration

import (
	"context"
	"fmt"
	"testing"

	"github.com/CrimsonCoder42/red_lite_cache/reader"
)

// TestOverflowWipesEveryReader: A, cut off by a proxy, overflows its pending-delete queue (cap 3)
// with deletes of keys that do not exist. Its replay is one delete_prefix "" on the real leader,
// which must empty the server and every OTHER reader of the sheet (B, C: connected throughout, so
// they only learn of it through the broadcast), not only A.
func TestOverflowWipesEveryReader(t *testing.T) {
	srv := StartLeader(t)
	proxy := NewProxy(t, srv.Addr)
	name := UniqueSheet(t)
	h := &attemptCounter{}
	a := NewReaderAt(t, proxyWS(proxy), "", name, fastReconnect(h), func(c *reader.Config) { c.MaxPendingDeletes = 3 })
	b := NewReader(t, srv, name)
	c := NewReader(t, srv, name)
	ctx := context.Background()

	keys := []string{"user:1:profile", "condition:patient:p1:list", "user:2:t:permissions", "plain"}
	for _, k := range keys {
		if err := b.Set(ctx, k, k); err != nil {
			t.Fatalf("Set %s: %v", k, err)
		}
	}
	for i, r := range []*reader.Reader{a, b, c} {
		Eventually(t, fmt.Sprintf("reader %d holds every key", i), func() bool {
			for _, k := range keys {
				if !r.Has(k) {
					return false
				}
			}
			return true
		})
	}

	proxy.Block()
	Eventually(t, "A disconnected", func() bool { return !a.IsConnected() })
	for i := range 5 { // none of these keys exist: only the collapsed delete_prefix "" can remove keys
		if err := a.Delete(ctx, fmt.Sprintf("absent:%d", i)); err != nil {
			t.Fatalf("Delete while down: %v", err)
		}
	}
	if n := a.Stats().PendingDeletes; n != 1 {
		t.Fatalf("PendingDeletes after overflow = %d, want 1 (delete_prefix \"\")", n)
	}
	raw, st := OpenRaw(t, srv, name)
	if len(st.Data) != len(keys) {
		t.Fatalf("server lost keys before the replay: %d of %d", len(st.Data), len(keys))
	}
	for _, k := range keys {
		if !b.Has(k) || !c.Has(k) {
			t.Fatalf("B or C lost %s before the replay", k)
		}
	}

	proxy.Unblock()
	Eventually(t, "A reconnects with an empty queue", func() bool { return a.IsConnected() && a.Stats().PendingDeletes == 0 })

	serverMisses(t, raw, keys...)
	if _, st := OpenRaw(t, srv, name); len(st.Data) != 0 {
		t.Fatalf("server sheet_state after the wipe holds %d keys", len(st.Data))
	}
	for i, r := range []*reader.Reader{a, b, c} {
		Eventually(t, fmt.Sprintf("reader %d empty after the wipe", i), func() bool {
			for _, k := range keys {
				if r.Has(k) {
					return false
				}
			}
			return r.Stats().Entries == 0
		})
	}
	// The sheet still works after the wipe: a new write reaches every reader.
	if err := c.Set(ctx, "after", 1); err != nil {
		t.Fatalf("Set after wipe: %v", err)
	}
	for i, r := range []*reader.Reader{a, b} {
		Eventually(t, fmt.Sprintf("reader %d sees the write after the wipe", i), func() bool { return r.Has("after") })
	}
}
