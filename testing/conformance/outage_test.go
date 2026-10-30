// Covers: backendAdvo cache layer
package conformance

// C7 (TTL bounds), C8 (an instance cut off while another writes and sweeps), C9 (an instance that
// boots while the cache is down). backendAdvo's DROPPED SWEEPS queue + linkReady + replay
// (cache/store.go:340-520) exist for C8; the consumer here queues nothing.

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/CrimsonCoder42/red_lite_cache/internal/config"
	"github.com/CrimsonCoder42/red_lite_cache/protocol"
	"github.com/CrimsonCoder42/red_lite_cache/reader"
	"github.com/CrimsonCoder42/red_lite_cache/testing/integration"
)

// TestC7TTLBounds: an analytics-class entry expires on every instance and the next read loads
// again; an entry with no TTL under CacheTTL=0 is still on every instance when it does.
//
// backendAdvo writes analytics with TTLAnalytics = 5 minutes (keys.go:2553). A test cannot wait
// five minutes, so the leader's REDLITE_CACHE_TTL is 300ms and the read-through passes ttl 0,
// which takes the server default (config.CacheTTL, D-003) — the same expiry path a 5-minute TTL
// takes, shortened.
func TestC7TTLBounds(t *testing.T) {
	cfForModes(t, func(t *testing.T, m cfMode) {
		short := cfStart(t, m, func(c *config.Config) { c.CacheTTL = 300 * time.Millisecond })
		never := cfStart(t, m, func(c *config.Config) { c.CacheTTL = 0 })
		p, tn := uuid.New(), uuid.New()
		var analytics Shape
		for _, s := range Corpus {
			if s.Name == "vitals.range_analytics" {
				analytics = s
			}
		}
		key := analytics.Sample(p, tn, 0)
		for _, c := range []*cfCluster{short, never} {
			c.db.Put(p, Row{Profile: p, Value: 5})
			c.readList(c.st[0], key, p, 0)
			c.seenEverywhere(key, 5)
			c.expectLoads(1, "first read")
		}

		for i, st := range short.st {
			integration.Eventually(t, fmt.Sprintf("instance %d sees %s expire", i, key), func() bool { return !st.R.Has(key) })
		}
		for i, st := range never.st {
			if v, ok := cachedValue(st, key); !ok || v != 5 {
				t.Fatalf("CacheTTL=0: instance %d lost %s (ok=%v v=%d) while the short-TTL entry expired", i, key, ok, v)
			}
		}
		never.expectLoads(1, "the no-TTL entry was never reloaded")
		if v := value(short.readList(short.st[1], key, p, 0)); v != 5 {
			t.Fatalf("reload after expiry = %d", v)
		}
		short.expectLoads(2, "the read after expiry goes to the database")
	})
}

// TestC8DisconnectRetiresDroppedSweeps: instance 2 is behind a proxy. Cut off, it reads misses
// (never the old value); instance 1 writes and sweeps; instance 2's own sweep while down returns
// nil and is queued by the reader; on reconnect the queued sweep is applied everywhere and
// instance 2's first read loads the new row. No stale value is readable on any instance at any
// step, and the consumer layer queued nothing (Store is the reader and nothing else).
func TestC8DisconnectRetiresDroppedSweeps(t *testing.T) {
	if n := reflect.TypeFor[Store]().NumField(); n != 1 {
		t.Fatalf("Store has %d fields: the consumer must hold no sweep queue (backendAdvo store.go:340-520)", n)
	}
	cfForModes(t, func(t *testing.T, m cfMode) {
		c := cfStart(t, m)
		ctx := context.Background()
		proxy := integration.NewProxy(t, c.srv.Addr)
		c.st[1] = c.storeAt("ws://" + proxy.Addr + protocol.PathWS)
		p := uuid.New()
		c.db.Put(p, Row{Profile: p, Value: 1})
		key := ConditionListKey(p, time.Time{}, uuid.Nil, 20) // swept by instance 1
		s, e := win(2)
		own := DomainFeedWindowKey(DomainPatientPrefix("measurement", p), s, e) // swept by instance 2 while down
		for _, k := range []string{key, own} {
			c.readList(c.st[0], k, p, TTLVitals)
			c.seenEverywhere(k, 1)
		}
		c.expectLoads(2, "warm")

		proxy.Block()
		integration.Eventually(t, "instance 2 notices the cut", func() bool { return !c.st[1].R.IsConnected() })
		for _, k := range []string{key, own} {
			if c.st[1].R.Has(k) {
				t.Fatalf("instance 2 serves %s while disconnected", k)
			}
		}

		c.db.Put(p, Row{Profile: p, Value: 2})
		if err := c.st[0].InvalidateDomain(ctx, "condition", p); err != nil {
			t.Fatalf("instance 1 sweep: %v", err)
		}
		c.barrier(c.st[0], c.st[2])
		c.neverStale(key, 1, "instance 1 swept, instance 2 down")
		if err := c.st[1].InvalidateDomain(ctx, "measurement", p); err != nil {
			t.Fatalf("instance 2's sweep while down = %v, want nil (queued by the reader)", err)
		}
		if v := value(c.readList(c.st[1], key, p, TTLVitals)); v != 2 {
			t.Fatalf("instance 2 read %d while down, want 2 from the database", v)
		}
		c.neverStale(key, 1, "instance 2 read through while down")
		if !c.st[0].R.Has(own) || !c.st[2].R.Has(own) {
			t.Fatal("instance 2's queued sweep reached the others before it reconnected")
		}

		loads := c.db.Loads()
		proxy.Unblock()
		integration.Eventually(t, "instance 2 reconnects", c.st[1].R.IsConnected)
		c.neverStale(key, 1, "instance 2 reconnected")
		if v := value(c.readList(c.st[1], key, p, TTLVitals)); v != 2 {
			t.Fatalf("instance 2's first read after reconnect = %d, want 2", v)
		}
		if c.db.Loads() <= loads {
			t.Fatal("instance 2's first read after reconnect did not miss")
		}
		c.barrier()
		c.neverStale(key, 1, "after reconnect")
		c.neverStale(own, 1, "after reconnect")
		for i, st := range c.st {
			if st.R.Has(own) {
				t.Fatalf("instance %d still holds %s: instance 2's queued sweep was not replayed", i, own)
			}
		}
	})
}

// TestC9BootWhileCacheDown: reader.New against an unreachable cache returns a usable reader within
// ConnectTimeout (backendAdvo's Logger.Fatal on a New error, cmd/api/main.go:112-120, never
// fires); reads go to the database; once the cache is reachable the instance connects and caches.
func TestC9BootWhileCacheDown(t *testing.T) {
	cfForModes(t, func(t *testing.T, m cfMode) {
		c := cfStart(t, m)
		proxy := integration.NewProxy(t, c.srv.Addr)
		proxy.Block()
		const connectTimeout = 300 * time.Millisecond
		start := time.Now()
		late := c.storeAt("ws://"+proxy.Addr+protocol.PathWS, func(rc *reader.Config) { rc.ConnectTimeout = connectTimeout })
		if took := time.Since(start); took > connectTimeout+2*time.Second {
			t.Fatalf("reader.New took %s with ConnectTimeout %s", took, connectTimeout)
		}
		if late.R.IsConnected() {
			t.Fatal("connected through a blocked proxy")
		}
		p := uuid.New()
		c.db.Put(p, Row{Profile: p, Value: 3})
		key := ConditionListKey(p, time.Time{}, uuid.Nil, 20)
		if v := value(c.readList(late, key, p, TTLVitals)); v != 3 {
			t.Fatalf("read while down = %d", v)
		}
		c.expectLoads(1, "the read while down goes to the database")
		c.barrier()
		for i, st := range c.st {
			if st.R.Has(key) {
				t.Fatalf("instance %d holds a value set while the booting instance was down", i)
			}
		}

		proxy.Unblock()
		integration.Eventually(t, "the late instance connects", late.R.IsConnected)
		c.readList(late, key, p, TTLVitals)
		c.expectLoads(2, "first read once connected misses")
		c.seenEverywhere(key, 3)
		c.readList(late, key, p, TTLVitals)
		for _, st := range c.st {
			c.readList(st, key, p, TTLVitals)
		}
		c.expectLoads(2, "then cached for every instance")
	})
}
