// Covers: backendAdvo cache layer
package conformance

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/CrimsonCoder42/red_lite_cache/internal/config"
	"github.com/CrimsonCoder42/red_lite_cache/protocol"
	"github.com/CrimsonCoder42/red_lite_cache/reader"
	"github.com/CrimsonCoder42/red_lite_cache/reader/v1compat"
	"github.com/CrimsonCoder42/red_lite_cache/testing/integration"
)

// newV1 opens a v1compat reader on sheet with the harness password and a fast first backoff step.
// after backendAdvo/cache/store.go:125 (reader.New(reader.Config{...})): same four v1 fields.
func newV1(t *testing.T, addr, sheet string, mods ...func(*v1compat.Config)) *v1compat.Reader {
	t.Helper()
	cfg := v1compat.Config{LeaderAddr: addr, Sheet: sheet, Password: "pw", ReconnectDelay: 10 * time.Millisecond}
	for _, m := range mods {
		m(&cfg)
	}
	r, err := v1compat.New(cfg)
	if err != nil {
		t.Fatalf("v1compat.New(%s): %v", sheet, err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

// TestC12V1CompatSurface: every v1 method against a live leader with three v1compat readers on one
// sheet (D-035). Each subtest names the v1 behaviour it reproduces or the documented difference.
func TestC12V1CompatSurface(t *testing.T) {
	srv := integration.StartLeader(t)
	sheet := integration.UniqueSheet(t)
	rs := []*v1compat.Reader{newV1(t, srv.WSURL, sheet), newV1(t, srv.WSURL, sheet), newV1(t, srv.WSURL, sheet)}
	w, r2, r3 := rs[0], rs[1], rs[2]
	ctx := context.Background()
	for i, r := range rs {
		if !r.IsConnected() {
			t.Fatalf("reader %d not connected after New", i)
		}
	}
	seenBy := func(what string, cond func(*v1compat.Reader) bool) {
		t.Helper()
		for _, r := range rs {
			integration.Eventually(t, what, func() bool { return cond(r) })
		}
	}

	t.Run("Get object is map[string]any, fresh per call", func(t *testing.T) {
		if err := w.Set(ctx, "obj", map[string]any{"name": "a", "n": 1}); err != nil {
			t.Fatalf("Set: %v", err)
		}
		seenBy("obj on every reader", func(r *v1compat.Reader) bool { return r.Has("obj") })
		for i, r := range rs {
			v, ok := r.Get("obj")
			m, isMap := v.(map[string]any)
			if !ok || !isMap || m["name"] != "a" || m["n"] != float64(1) {
				t.Fatalf("reader %d Get(obj) = %#v %v, want map{name:a n:1}", i, v, ok)
			}
			m["name"] = "mutated"
			if again, _ := r.GetMap("obj"); again["name"] != "a" {
				t.Fatalf("reader %d: mutating Get's map changed the next GetMap: %v", i, again)
			}
		}
		if s, ok := r2.GetString("obj"); ok || s != "" {
			t.Fatalf("GetString(object) = %q %v, want \"\" false (v1 type rule)", s, ok)
		}
	})

	t.Run("GetString and GetMap type rules", func(t *testing.T) {
		if err := w.SetWithTTL(ctx, "str", "hello", 5); err != nil {
			t.Fatalf("SetWithTTL: %v", err)
		}
		if err := w.Set(ctx, "num", 42); err != nil {
			t.Fatalf("Set num: %v", err)
		}
		seenBy("str and num", func(r *v1compat.Reader) bool { return r.Has("str") && r.Has("num") })
		if s, ok := r3.GetString("str"); !ok || s != "hello" {
			t.Fatalf("GetString(str) = %q %v", s, ok)
		}
		if m, ok := r3.GetMap("str"); ok || m != nil {
			t.Fatalf("GetMap(string) = %v %v, want nil false", m, ok)
		}
		if v, ok := r3.Get("num"); !ok || v != float64(42) {
			t.Fatalf("Get(num) = %#v %v, want float64(42)", v, ok)
		}
		if _, ok := r3.GetString("num"); ok {
			t.Fatal("GetString(number) ok, want false")
		}
		if _, ok := r3.Get("absent"); ok || r3.Has("absent") || r3.IsNull("absent") {
			t.Fatal("absent key reported present")
		}
	})

	t.Run("null placeholder via raw set_null", func(t *testing.T) {
		raw, _ := integration.OpenRaw(t, srv, sheet)
		if resp := raw.Do(protocol.Request{Action: protocol.ActionSetNull, ID: 1, Key: "nul", TTL: 5}); !resp.Success {
			t.Fatalf("set_null = %+v", resp)
		}
		seenBy("nul placeholder", func(r *v1compat.Reader) bool { return r.IsNull("nul") })
		for i, r := range rs {
			v, ok := r.Get("nul")
			s, sok := r.GetString("nul")
			if !ok || v != protocol.NullPlaceholder || !sok || s != protocol.NullPlaceholder || !r.Has("nul") {
				t.Fatalf("reader %d null: Get=%#v %v GetString=%q %v Has=%v", i, v, ok, s, sok, r.Has("nul"))
			}
		}
	})

	t.Run("Delete DeletePrefix DeleteSuffix reach every reader", func(t *testing.T) {
		keys := []string{"vit:patient:u1:bp", "vit:patient:u1:hr", "vit:patient:u2:bp", "user:t1:permissions", "single"}
		for _, k := range keys {
			if err := w.Set(ctx, k, k); err != nil {
				t.Fatalf("Set %s: %v", k, err)
			}
		}
		seenBy("seed keys", func(r *v1compat.Reader) bool { return r.Has("single") && r.Has("vit:patient:u2:bp") })
		if err := r2.DeletePrefix(ctx, "vit:patient:u1"); err != nil {
			t.Fatalf("DeletePrefix: %v", err)
		}
		if err := r3.DeleteSuffix(ctx, ":permissions"); err != nil {
			t.Fatalf("DeleteSuffix: %v", err)
		}
		if err := w.Delete(ctx, "single"); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		seenBy("deletes applied", func(r *v1compat.Reader) bool {
			return !r.Has("vit:patient:u1:bp") && !r.Has("vit:patient:u1:hr") && !r.Has("user:t1:permissions") && !r.Has("single")
		})
		for i, r := range rs {
			if !r.Has("vit:patient:u2:bp") {
				t.Fatalf("reader %d lost a key outside the sweep", i)
			}
		}
	})

	t.Run("stale set returns nil like v1 fire-and-forget, nothing cached", func(t *testing.T) {
		// Control: the plain reader reports ErrStale for the same sequence.
		plain := integration.NewReader(t, srv, sheet)
		var x string
		_ = plain.Get("stale0", &x) // the miss a read-through starts with
		if err := r2.Delete(ctx, "stale0"); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if err := plain.Set(ctx, "stale0", "old"); !errors.Is(err, reader.ErrStale) {
			t.Fatalf("control: plain reader Set after a covering delete = %v, want ErrStale", err)
		}
		_, _ = w.Get("stale1")
		if err := r2.Delete(ctx, "stale1"); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if err := w.Set(ctx, "stale1", "old"); err != nil {
			t.Fatalf("v1compat Set after a covering delete = %v, want nil (ErrStale mapped)", err)
		}
		if w.Has("stale1") {
			t.Fatal("stale value was cached")
		}
	})

	t.Run("CurrentServer and CacheSize approximations", func(t *testing.T) {
		if got := w.CurrentServer(); got != srv.WSURL {
			t.Fatalf("CurrentServer = %q, want the configured LeaderAddr %q", got, srv.WSURL)
		}
		if got := w.CacheSize(); got != -1 {
			t.Fatalf("CacheSize = %d, want -1 (unknown)", got)
		}
	})

	t.Run("wrong password is ErrAuthFailed as in v1", func(t *testing.T) {
		_, err := v1compat.New(v1compat.Config{LeaderAddr: srv.WSURL, Sheet: sheet, Password: "wrong"})
		if !errors.Is(err, v1compat.ErrAuthFailed) || !errors.Is(err, reader.ErrAuthFailed) {
			t.Fatalf("New with a wrong password = %v, want ErrAuthFailed", err)
		}
	})

	t.Run("after Close: ErrClosed, reads miss, Close idempotent", func(t *testing.T) {
		if !r3.Has("str") {
			t.Fatal("precondition: str present")
		}
		if err := r3.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if err := r3.Set(ctx, "k", 1); !errors.Is(err, v1compat.ErrClosed) || !errors.Is(err, reader.ErrClosed) {
			t.Fatalf("Set after Close = %v, want ErrClosed", err)
		}
		for name, err := range map[string]error{
			"Delete": r3.Delete(ctx, "k"), "DeletePrefix": r3.DeletePrefix(ctx, "k"), "DeleteSuffix": r3.DeleteSuffix(ctx, "k"),
		} {
			if !errors.Is(err, v1compat.ErrClosed) {
				t.Fatalf("%s after Close = %v, want ErrClosed", name, err)
			}
		}
		// v1 kept serving its last copy after Close (reader.go:154-159 never checked closed).
		if _, ok := r3.Get("str"); ok || r3.IsConnected() {
			t.Fatal("closed reader served a read or reports connected")
		}
		if err := r3.Close(); err != nil {
			t.Fatalf("second Close = %v, want nil", err)
		}
	})
}

// TestC12V1NewAgainstNothing: v1 New returned the dial error when no server answered
// (advo_cache/reader/reader.go:121-148); v1compat New returns a disconnected, reconnecting reader
// after the connect timeout (R10). While disconnected: Set is ErrNotConnected (as v1), deletes are
// queued and return nil (v1: ErrNotConnected, dropped), reads miss.
func TestC12V1NewAgainstNothing(t *testing.T) {
	ln := integration.Listen(t)
	addr := "ws://" + ln.Addr().String() + protocol.PathWS
	ln.Close() // nothing listens: every dial is refused
	start := time.Now()
	r := newV1(t, addr, integration.UniqueSheet(t))
	if r.IsConnected() {
		t.Fatal("connected to nothing")
	}
	if waited := time.Since(start); waited < reader.DefaultConnectTimeout {
		t.Fatalf("New returned after %s, before the connect timeout %s", waited, reader.DefaultConnectTimeout)
	}
	ctx := context.Background()
	if err := r.Set(ctx, "k", 1); !errors.Is(err, v1compat.ErrNotConnected) || !errors.Is(err, reader.ErrNotConnected) {
		t.Fatalf("Set while disconnected = %v, want ErrNotConnected", err)
	}
	if err := r.Delete(ctx, "k"); err != nil {
		t.Fatalf("Delete while disconnected = %v, want nil (queued)", err)
	}
	if err := r.DeletePrefix(ctx, "vit:patient:u1"); err != nil {
		t.Fatalf("DeletePrefix while disconnected = %v, want nil (queued)", err)
	}
	if _, ok := r.Get("k"); ok || r.Has("k") || r.IsNull("k") {
		t.Fatal("disconnected reader served a read")
	}
}

// TestC12V1MaxReconnectAttemptsIgnored: with MaxReconnectAttempts 1, v1 gave up after one try per
// server (reader.go:544-584; its New did not retry at all). v1compat keeps trying (R1): the first
// three attempts are refused (accepted and closed before the upgrade), then a leader comes up on
// the same address and the same reader connects and serves broadcasts.
func TestC12V1MaxReconnectAttemptsIgnored(t *testing.T) {
	gate := integration.Listen(t)
	hostport := gate.Addr().String()
	const refuse = 3
	var refused atomic.Int32
	refusedAll := make(chan struct{})
	go func() {
		defer close(refusedAll)
		defer gate.Close()
		for refused.Load() < refuse {
			c, err := gate.Accept()
			if err != nil {
				return
			}
			c.Close()
			refused.Add(1)
		}
	}()
	sheet := integration.UniqueSheet(t)
	type made struct {
		r   *v1compat.Reader
		err error
	}
	newDone := make(chan made, 1)
	go func() {
		r, err := v1compat.New(v1compat.Config{
			LeaderAddr: "ws://" + hostport + protocol.PathWS, Sheet: sheet, Password: "pw",
			MaxReconnectAttempts: 1, ReconnectDelay: 10 * time.Millisecond,
		})
		newDone <- made{r, err}
	}()
	select {
	case <-refusedAll:
	case <-time.After(integration.Timeout):
		t.Fatalf("only %d connect attempts within %s", refused.Load(), integration.Timeout)
	}
	if n := refused.Load(); n < refuse {
		t.Fatalf("refused %d attempts, want %d", n, refuse)
	}
	ln, err := net.Listen("tcp", hostport)
	if err != nil {
		t.Fatalf("relisten %s: %v", hostport, err)
	}
	srv := integration.StartOn(t, ln, leaderConfig(t))
	m := <-newDone
	if m.err != nil {
		t.Fatalf("v1compat.New: %v", m.err)
	}
	r := m.r
	t.Cleanup(func() { r.Close() })
	integration.Eventually(t, "reader connected after refused attempts", r.IsConnected)
	w := newV1(t, srv.WSURL, sheet)
	if err := w.Set(context.Background(), "after", "x"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	integration.Eventually(t, "write reaches the reconnected reader", func() bool { return r.Has("after") })
}

// leaderConfig is integration.TestConfig as a leader.
func leaderConfig(t *testing.T) config.Config {
	cfg := integration.TestConfig(t)
	cfg.Role = "leader"
	return cfg
}
