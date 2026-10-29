// Covers: reader R1 R2 R3 R10 (reconnect forever, delete queue + replay, disconnected reads miss, New while down)
package integration

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/CrimsonCoder42/red_lite_cache/protocol"
	"github.com/CrimsonCoder42/red_lite_cache/reader"
)

// attemptCounter is a slog.Handler that records the highest "attempt" of every
// "Reader: connect failed" line. The reader logs attempts 1, 2, 4, 8, 16, 32, … so a value of 32
// proves at least 32 failed connects in a row.
type attemptCounter struct{ max atomic.Int64 }

func (h *attemptCounter) Enabled(context.Context, slog.Level) bool { return true }
func (h *attemptCounter) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *attemptCounter) WithGroup(string) slog.Handler            { return h }
func (h *attemptCounter) Handle(_ context.Context, rec slog.Record) error {
	if rec.Message != "Reader: connect failed" {
		return nil
	}
	rec.Attrs(func(a slog.Attr) bool {
		if a.Key == "attempt" {
			n := a.Value.Int64()
			for {
				cur := h.max.Load()
				if n <= cur || h.max.CompareAndSwap(cur, n) {
					break
				}
			}
			return false
		}
		return true
	})
	return nil
}

// fastReconnect is the reader config every test here uses: 5ms..20ms backoff and a logger that
// counts failed attempts.
func fastReconnect(h *attemptCounter) func(*reader.Config) {
	return func(c *reader.Config) {
		c.ReconnectMin, c.ReconnectMax = 5*time.Millisecond, 20*time.Millisecond
		c.Logger = slog.New(h)
	}
}

func proxyWS(p *Proxy) string { return "ws://" + p.Addr + protocol.PathWS }

// serverMisses fails unless the leader answers key not found for every key.
func serverMisses(t *testing.T, raw *RawConn, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if g := raw.Do(protocol.Request{Action: protocol.ActionGet, Key: k}); g.Success || g.Error != protocol.ErrMsgKeyNotFound {
			t.Fatalf("server get %s = %+v, want key not found", k, g)
		}
	}
}

// TestDisconnectLoop is the DISCONNECT LOOP (R1–R3): A is cut off from the leader by a proxy, its
// reads miss, its deletes are queued, it keeps reconnecting, and the moment it is connected again
// the queued deletes have been applied on the server and the reloaded sheet_state did not
// resurrect them locally.
func TestDisconnectLoop(t *testing.T) {
	srv := StartLeader(t)
	proxy := NewProxy(t, srv.Addr)
	name := UniqueSheet(t)
	h := &attemptCounter{}
	a := NewReaderAt(t, proxyWS(proxy), "", name, fastReconnect(h))
	b := NewReader(t, srv, name)
	ctx := context.Background()

	const k, pk, sk, keep = "user:1:k", "pre:x", "x:suf", "other"
	for _, key := range []string{k, pk, sk, keep} {
		if err := a.Set(ctx, key, key); err != nil {
			t.Fatalf("Set %s: %v", key, err)
		}
	}
	Eventually(t, "B sees every key", func() bool {
		return b.Has(k) && b.Has(pk) && b.Has(sk) && b.Has(keep)
	})
	var s string
	if !a.Get(k, &s) || s != k {
		t.Fatalf("A.Get(%s) = %q before the cut", k, s)
	}

	proxy.Block()
	Eventually(t, "A disconnected", func() bool { return !a.IsConnected() })

	// R3: A's map still holds k (nothing deleted it), yet a read misses.
	if a.Get(k, &s) || a.Has(k) || a.Has(keep) {
		t.Fatal("A served a read while disconnected (R3)")
	}

	// R2: deletes while down return nil and are queued.
	if err := a.Delete(ctx, k); err != nil {
		t.Fatalf("Delete while down: %v", err)
	}
	if err := a.DeletePrefix(ctx, "pre:"); err != nil {
		t.Fatalf("DeletePrefix while down: %v", err)
	}
	if err := a.DeleteSuffix(ctx, ":suf"); err != nil {
		t.Fatalf("DeleteSuffix while down: %v", err)
	}

	// R1: hold the partition through many attempts (at most 20ms apart). The server never got the
	// deletes, so B still sees every key throughout.
	Eventually(t, "A fails at least 16 connects in a row", func() bool { return h.max.Load() >= 16 })
	if a.IsConnected() {
		t.Fatal("A connected through a blocked proxy")
	}
	for _, key := range []string{k, pk, sk, keep} {
		if !b.Has(key) {
			t.Fatalf("B lost %s although the server never got a delete", key)
		}
	}

	proxy.Unblock()
	Eventually(t, "A reconnects", a.IsConnected)

	// The moment A is connected the replay is acked: no Eventually on the server or on A.
	raw, _ := OpenRaw(t, srv, name)
	serverMisses(t, raw, k, pk, sk)
	if g := raw.Do(protocol.Request{Action: protocol.ActionGet, Key: keep}); !g.Success {
		t.Fatalf("server get %s = %+v, want the untouched key kept", keep, g)
	}
	for _, key := range []string{k, pk, sk} {
		if a.Has(key) {
			t.Fatalf("A has %s after reconnect: the sheet_state resurrected a queued delete (R2)", key)
		}
	}
	if !a.Get(keep, &s) || s != keep {
		t.Fatalf("A.Get(%s) after reconnect = %q, want served again", keep, s)
	}
	Eventually(t, "B misses the deleted keys", func() bool { return !b.Has(k) && !b.Has(pk) && !b.Has(sk) })
	if !b.Has(keep) {
		t.Fatal("B lost the untouched key")
	}
}

// TestNewWhileLeaderDown (R10): New against an address that refuses (blocked proxy) or does not
// listen (closed port) returns the reader after ConnectTimeout instead of failing, disconnected; the
// blocked one connects once unblocked.
func TestNewWhileLeaderDown(t *testing.T) {
	const connectTimeout = 300 * time.Millisecond
	ctx := context.Background()

	t.Run("blocked", func(t *testing.T) {
		srv := StartLeader(t)
		proxy := NewProxy(t, srv.Addr)
		proxy.Block()
		name := UniqueSheet(t)
		start := time.Now()
		a := NewReaderAt(t, proxyWS(proxy), "", name, fastReconnect(&attemptCounter{}), func(c *reader.Config) {
			c.ConnectTimeout = connectTimeout
		})
		if el := time.Since(start); el < connectTimeout || el > connectTimeout+time.Second {
			t.Fatalf("New returned after %s, want about %s", el, connectTimeout)
		}
		if a.IsConnected() || a.Has("k") {
			t.Fatal("reader connected or served a read through a blocked proxy")
		}
		if err := a.Set(ctx, "k", "v"); !errors.Is(err, reader.ErrNotConnected) {
			t.Fatalf("Set while down = %v, want ErrNotConnected", err)
		}
		proxy.Unblock()
		Eventually(t, "A connects after unblock", a.IsConnected)
		if err := a.Set(ctx, "k", "v"); err != nil {
			t.Fatalf("Set after connect: %v", err)
		}
		b := NewReader(t, srv, name)
		var s string
		if !b.Get("k", &s) || s != "v" {
			t.Fatalf("B.Get(k) = %q, want v from A's set", s)
		}
	})

	t.Run("stopped", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		addr := ln.Addr().String()
		ln.Close() // nothing listens here now
		start := time.Now()
		a := NewReaderAt(t, "ws://"+addr+protocol.PathWS, "", UniqueSheet(t), fastReconnect(&attemptCounter{}), func(c *reader.Config) {
			c.ConnectTimeout = connectTimeout
		})
		if el := time.Since(start); el < connectTimeout || el > connectTimeout+time.Second {
			t.Fatalf("New returned after %s, want about %s", el, connectTimeout)
		}
		if a.IsConnected() {
			t.Fatal("connected to a closed port")
		}
	})
}

// TestReconnectForever (R1): with 5ms..20ms backoff the reader fails more than 20 connects in a row
// (v1 gave up after 3 per server) and still connects when the leader comes back.
func TestReconnectForever(t *testing.T) {
	srv := StartLeader(t)
	proxy := NewProxy(t, srv.Addr)
	name := UniqueSheet(t)
	h := &attemptCounter{}
	a := NewReaderAt(t, proxyWS(proxy), "", name, fastReconnect(h))
	if !a.IsConnected() {
		t.Fatal("A not connected at start")
	}
	proxy.Block()
	Eventually(t, "A fails at least 32 connects in a row", func() bool { return h.max.Load() >= 32 })
	proxy.Unblock()
	Eventually(t, "A reconnects after 32+ failures", a.IsConnected)
	if err := a.Set(context.Background(), "k", "v"); err != nil {
		t.Fatalf("Set after reconnect: %v", err)
	}
	raw, _ := OpenRaw(t, srv, name)
	if g := raw.Do(protocol.Request{Action: protocol.ActionGet, Key: "k"}); !g.Success || string(g.Value) != `"v"` {
		t.Fatalf("server get k = %+v", g)
	}
}
