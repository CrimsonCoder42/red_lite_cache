package reader_test

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/CrimsonCoder42/red_lite_cache/internal/config"
	"github.com/CrimsonCoder42/red_lite_cache/protocol"
	"github.com/CrimsonCoder42/red_lite_cache/testing/integration"
)

// TestWireExpiryBroadcast (S9 criterion 6, D-045): black-box over the wire, v1 frames only. A key
// set with the server's default TTL (2 s here) is gone from get, and every other session of the
// sheet receives invalidate{keys} naming it, within TTL + 1 s (the wheel's bucket) + the janitor
// interval — plus 2 s of slack for a loaded -race run.
func TestWireExpiryBroadcast(t *testing.T) {
	const ttl = 2 * time.Second
	srv := integration.StartLeader(t, func(c *config.Config) { c.CacheTTL = ttl })
	sheet := integration.UniqueSheet(t)
	w, _ := integration.OpenRaw(t, srv, sheet)
	obs, _ := integration.OpenRaw(t, srv, sheet)

	start := time.Now()
	if r := w.Do(protocol.Request{Action: protocol.ActionSet, Key: "exp:1", Value: json.RawMessage(`1`)}); !r.Success || r.ExpiresInMS <= 0 {
		t.Fatalf("set: %+v", r)
	}
	limit := ttl + time.Second + srv.Cfg.CacheJanitorInterval + 2*time.Second
	for {
		left := limit - time.Since(start)
		if left <= 0 {
			t.Fatalf("no invalidate{keys:[exp:1]} within %v", limit)
		}
		b, err := obs.TryNext(left)
		if err != nil {
			t.Fatalf("no invalidate{keys:[exp:1]} within %v: %v", limit, err)
		}
		var inv protocol.Invalidate
		if json.Unmarshal(b, &inv) == nil && inv.Type == protocol.TypeInvalidate && slices.Contains(inv.Keys, "exp:1") {
			break
		}
	}
	took := time.Since(start)
	if took < ttl {
		t.Fatalf("expired after %v, before its %v TTL", took, ttl)
	}
	if r := w.Do(protocol.Request{Action: protocol.ActionGet, Key: "exp:1"}); r.Success {
		t.Fatalf("get after expiry: %+v", r)
	}
	t.Logf("invalidate{keys} %v after the set (TTL %v)", took.Round(time.Millisecond), ttl)
}
