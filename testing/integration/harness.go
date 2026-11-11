package integration

// Harness helpers every integration test uses (D-017). This file holds the pieces that do not boot a
// server: a raw frame client, a cuttable TCP proxy, unique sheet names and the persist skip.
// StartLeader / StartReplica live in harness_server.go.

import (
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/CrimsonCoder42/red_lite_cache/internal/config"
)

// Timeout bounds every wait in a test so a hang fails instead of stalling the suite.
const Timeout = 10 * time.Second

// SkipPersist is the exact skip line testing/run.sh greps for.
const SkipPersist = "SKIPPED: persist (no REDLITE_TEST_DATABASE_URL)"

var sheetSeq atomic.Uint64

// UniqueSheet returns a sheet name no other test in this process uses.
func UniqueSheet(t testing.TB) string {
	t.Helper()
	name := strings.NewReplacer("/", "_", " ", "_").Replace(t.Name())
	return fmt.Sprintf("%s-%d", name, sheetSeq.Add(1))
}

// TestDatabaseURL returns REDLITE_TEST_DATABASE_URL (through internal/config) or skips the test
// with SkipPersist.
func TestDatabaseURL(t testing.TB) string {
	t.Helper()
	u := config.TestDatabaseURL()
	if u == "" {
		t.Skip(SkipPersist)
	}
	return u
}

// Proxy is a TCP forwarder in front of a server. Cut drops every live connection (the DISCONNECT
// loop's "kill the leader conn"); Block refuses new ones until Unblock (a partition).
type Proxy struct {
	Addr string // host:port to dial instead of the target

	target  string
	ln      net.Listener
	mu      sync.Mutex
	conns   map[net.Conn]struct{}
	blocked bool
	done    chan struct{}
	wg      sync.WaitGroup
}

// NewProxy listens on 127.0.0.1:0 and forwards to target (host:port). Stopped at cleanup.
func NewProxy(t testing.TB, target string) *Proxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	p := &Proxy{Addr: ln.Addr().String(), target: target, ln: ln, conns: map[net.Conn]struct{}{}, done: make(chan struct{})}
	p.wg.Go(p.accept)
	t.Cleanup(p.Close)
	return p
}

func (p *Proxy) accept() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		blocked := p.blocked
		p.mu.Unlock()
		if blocked {
			c.Close()
			continue
		}
		up, err := net.DialTimeout("tcp", p.target, Timeout)
		if err != nil {
			c.Close()
			continue
		}
		p.track(c, up)
		p.wg.Go(func() { p.pipe(c, up) })
		p.wg.Go(func() { p.pipe(up, c) })
	}
}

func (p *Proxy) track(cs ...net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range cs {
		p.conns[c] = struct{}{}
	}
}

func (p *Proxy) pipe(dst, src net.Conn) {
	io.Copy(dst, src)
	dst.Close()
	src.Close()
	p.mu.Lock()
	delete(p.conns, dst)
	delete(p.conns, src)
	p.mu.Unlock()
}

// Cut closes every connection currently through the proxy. New connections are accepted.
func (p *Proxy) Cut() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for c := range p.conns {
		c.Close()
	}
}

// Block cuts every connection and refuses new ones until Unblock.
func (p *Proxy) Block() {
	p.mu.Lock()
	p.blocked = true
	p.mu.Unlock()
	p.Cut()
}

// Unblock accepts new connections again.
func (p *Proxy) Unblock() {
	p.mu.Lock()
	p.blocked = false
	p.mu.Unlock()
}

// Close stops the proxy and every connection through it.
func (p *Proxy) Close() {
	select {
	case <-p.done:
		return
	default:
		close(p.done)
	}
	p.ln.Close()
	p.Cut()
	p.wg.Wait()
}

// Eventually polls cond every 10ms until it is true, failing the test after Timeout.
func Eventually(t testing.TB, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(Timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", Timeout, what)
}
