// Covers: open, set, set_null, delete, get, janitor expiry (invalidate keys)
package integration

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/CrimsonCoder42/red_lite_cache/internal/config"
	"github.com/CrimsonCoder42/red_lite_cache/protocol"
)

// TestKeyLoop is the KEY LOOP (R4, R5): a set on one reader reaches the other, a delete removes it
// everywhere, a default-TTL key expires on every reader and the janitor broadcasts invalidate{keys},
// and set_null reads as IsNull and a Get miss.
func TestKeyLoop(t *testing.T) {
	srv := StartLeader(t, func(c *config.Config) { c.CacheTTL = 300 * time.Millisecond })
	name := UniqueSheet(t)
	a := NewReader(t, srv, name)
	b := NewReader(t, srv, name)
	ctx := context.Background()

	// set with an explicit TTL on A → B sees it
	if err := a.SetWithTTL(ctx, "k1", "v1", 5); err != nil {
		t.Fatalf("SetWithTTL: %v", err)
	}
	Eventually(t, "B sees k1", func() bool {
		var s string
		return b.Get("k1", &s) && s == "v1"
	})

	// delete on A → B misses
	if err := a.Delete(ctx, "k1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	Eventually(t, "B misses k1", func() bool { return !b.Has("k1") })
	var s string
	if a.Get("k1", &s) {
		t.Fatal("A still has k1 after its own delete")
	}

	// ttl 0 → REDLITE_CACHE_TTL (300ms): every reader misses after expiry, and the janitor's
	// invalidate{keys} reaches a collaborator
	raw, _ := OpenRaw(t, srv, name)
	start := time.Now()
	if err := a.Set(ctx, "k2", "v2"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	var rep protocol.Replicate
	raw.NextType(protocol.TypeReplicate, &rep)
	if rep.Key != "k2" || rep.ExpiresInMS <= 0 || rep.ExpiresInMS > 300 || rep.Seq == 0 {
		t.Fatalf("replicate = %+v, want key k2, 0 < expires_in_ms <= 300, seq set", rep)
	}
	Eventually(t, "B sees k2", func() bool { return b.Has("k2") })
	var inv protocol.Invalidate
	raw.NextType(protocol.TypeInvalidate, &inv)
	if !slices.Contains(inv.Keys, "k2") || inv.Seq <= rep.Seq {
		t.Fatalf("invalidate = %+v, want keys [k2] at seq > %d", inv, rep.Seq)
	}
	if el := time.Since(start); el < 250*time.Millisecond {
		t.Fatalf("janitor expired k2 after %s, before its 300ms TTL", el)
	}
	Eventually(t, "A and B miss k2", func() bool { return !a.Has("k2") && !b.Has("k2") })
	if resp := raw.Do(protocol.Request{Action: protocol.ActionGet, Key: "k2"}); resp.Success || resp.Error != protocol.ErrMsgKeyNotFound {
		t.Fatalf("server get k2 after expiry = %+v", resp)
	}

	// set_null → IsNull true, Get miss
	resp := raw.Do(protocol.Request{Action: protocol.ActionSetNull, ID: 3, Key: "n", TTL: 5})
	if !resp.Success || resp.Key != "n" || resp.ID != 3 || resp.Seq == 0 {
		t.Fatalf("set_null reply = %+v", resp)
	}
	Eventually(t, "B IsNull(n)", func() bool { return b.IsNull("n") })
	if b.Get("n", &s) {
		t.Fatal("Get of a null placeholder hit")
	}
	if g := raw.Do(protocol.Request{Action: protocol.ActionGet, Key: "n"}); !g.Success || string(g.Value) != string(protocol.NullValue) {
		t.Fatalf("server get n = %+v", g)
	}
}
