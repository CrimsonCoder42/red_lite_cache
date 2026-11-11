// Covers: internal/rpc Conn over an in-memory Transport (queue bound, kick, close, ping, panics, audit)
package rpc

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/metadata"

	"github.com/CrimsonCoder42/red_lite_cache/internal/lock"
	"github.com/CrimsonCoder42/red_lite_cache/internal/session"
	"github.com/CrimsonCoder42/red_lite_cache/internal/sheet"
	"github.com/CrimsonCoder42/red_lite_cache/internal/tenant"
	"github.com/CrimsonCoder42/red_lite_cache/protocol"
	pb "github.com/CrimsonCoder42/red_lite_cache/protocol/redlitev1"
	"github.com/CrimsonCoder42/red_lite_cache/protocol/rpcwire"
)

// memTransport is a Transport whose peer is the test: in feeds Recv, out receives every written
// frame (a Write blocks while the test does not read out, like a peer that stops reading).
type memTransport struct {
	in      chan []byte
	out     chan []byte
	done    chan struct{}
	abort   sync.Once
	pingErr atomic.Value // error

	mu     sync.Mutex
	code   int
	reason string
	closed bool
}

func newMem(outCap int) *memTransport {
	return &memTransport{in: make(chan []byte, 16), out: make(chan []byte, outCap), done: make(chan struct{}), code: -1}
}

var errMemClosed = errors.New("mem transport closed")

func (m *memTransport) Recv() ([]byte, error) {
	select {
	case f := <-m.in:
		return f, nil
	case <-m.done:
		return nil, &CloseError{Code: m.closeCode(), Err: errMemClosed}
	}
}

func (m *memTransport) Write(ctx context.Context, frame []byte) error {
	select {
	case m.out <- frame:
		return nil
	case <-m.done:
		return errMemClosed
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *memTransport) Close(code int, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.closed {
		m.closed, m.code, m.reason = true, code, reason
	}
}

func (m *memTransport) Abort() { m.abort.Do(func() { close(m.done) }) }

func (m *memTransport) Ping(context.Context) error {
	if err, ok := m.pingErr.Load().(error); ok {
		return err
	}
	return nil
}

func (m *memTransport) closeCode() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.code
}

func (m *memTransport) closeInfo() (int, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.code, m.reason
}

// run starts a Conn on t with handle; it returns the conn once Run is running.
func run(t *testing.T, tr *memTransport, opts ConnOptions, handle func(c *Conn, f []byte)) *Conn {
	t.Helper()
	if opts.WriteTimeout == 0 {
		opts.WriteTimeout = time.Second
	}
	c := NewConn(tr, opts)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); c.Run(ctx, func(f []byte) { handle(c, f) }) }()
	t.Cleanup(func() { cancel(); <-done })
	return c
}

func next(t *testing.T, tr *memTransport) []byte {
	t.Helper()
	select {
	case f := <-tr.out:
		return f
	case <-time.After(5 * time.Second):
		t.Fatal("no frame within 5s")
	}
	return nil
}

func waitDone(t *testing.T, c *Conn) {
	t.Helper()
	select {
	case <-c.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("conn not done within 5s")
	}
}

// A panic in the frame handler is recovered, counted, and ends that conn only.
func TestConnPanicClosesOnlyThatConn(t *testing.T) {
	var panics atomic.Uint64
	h := func(c *Conn, f []byte) {
		if string(f) == "boom" {
			panic("boom")
		}
		c.Send(f)
	}
	ta, tb := newMem(4), newMem(4)
	a := run(t, ta, ConnOptions{SendQueue: 4, Panics: &panics}, h)
	b := run(t, tb, ConnOptions{SendQueue: 4, Panics: &panics}, h)
	if a.ID() == b.ID() {
		t.Fatal("conn ids not unique")
	}
	tb.in <- []byte("echo")
	if got := next(t, tb); string(got) != "echo" || panics.Load() != 0 {
		t.Fatalf("echo %q, panics %d", got, panics.Load())
	}
	ta.in <- []byte("boom")
	waitDone(t, a)
	if n := panics.Load(); n != 1 {
		t.Fatalf("Panics = %d, want 1", n)
	}
	select {
	case <-b.Done():
		t.Fatal("the other conn ended")
	default:
	}
	tb.in <- []byte("again")
	if got := next(t, tb); string(got) != "again" {
		t.Fatalf("other conn: %q", got)
	}
}

// Send is non-blocking and refuses once the queue is full or the conn is kicked; Kick is
// idempotent and closes with 1001.
func TestConnQueueBoundAndKick(t *testing.T) {
	tr := newMem(0) // a peer that never reads
	c := run(t, tr, ConnOptions{SendQueue: 2, WriteTimeout: time.Minute}, func(*Conn, []byte) {})
	refused := false
	for range 100 {
		if !c.Send([]byte("x")) {
			refused = true
			break
		}
	}
	if !refused {
		t.Fatal("Send never refused with a non-reading peer and a 2-frame queue")
	}
	c.Kick("test kick")
	c.Kick("again")
	if c.Send([]byte("x")) {
		t.Fatal("Send accepted after Kick")
	}
	waitDone(t, c)
	if code, reason := tr.closeInfo(); code != rpcwire.CloseGoingAway || reason != "test kick" {
		t.Fatalf("close %d %q, want 1001 %q", code, reason, "test kick")
	}
}

// The final frame is written before the close; later Sends are refused.
func TestConnSendAndClose(t *testing.T) {
	tr := newMem(8)
	c := run(t, tr, ConnOptions{SendQueue: 4}, func(c *Conn, f []byte) {
		c.Send([]byte("first"))
		c.SendAndClose([]byte("last"), rpcwire.ClosePolicyViolation, "bye")
		if c.Send([]byte("after")) {
			panic("Send accepted after SendAndClose")
		}
	})
	tr.in <- []byte("go")
	for _, want := range []string{"first", "last"} {
		if got := next(t, tr); string(got) != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	}
	waitDone(t, c)
	if code, _ := tr.closeInfo(); code != rpcwire.ClosePolicyViolation {
		t.Fatalf("close %d, want 1008", code)
	}
}

// CloseWith sends its code (the first call wins); after SendAndClose it does nothing, so the queued
// final frame and its code still reach the peer.
func TestConnCloseWith(t *testing.T) {
	t.Run("code reaches the peer", func(t *testing.T) {
		tr := newMem(4)
		c := run(t, tr, ConnOptions{SendQueue: 4}, func(*Conn, []byte) {})
		c.CloseWith(protocol.CloseKicked, protocol.CloseReasonKicked)
		c.CloseWith(protocol.CloseSuspended, "second call ignored")
		waitDone(t, c)
		if code, reason := tr.closeInfo(); code != protocol.CloseKicked || reason != protocol.CloseReasonKicked {
			t.Fatalf("close %d %q", code, reason)
		}
	})
	t.Run("does not drop a queued final frame", func(t *testing.T) {
		tr := newMem(4)
		c := run(t, tr, ConnOptions{SendQueue: 4}, func(c *Conn, f []byte) {
			c.SendAndClose([]byte("last"), rpcwire.ClosePolicyViolation, "bye")
			c.CloseWith(protocol.CloseKicked, protocol.CloseReasonKicked)
		})
		tr.in <- []byte("go")
		if got := next(t, tr); string(got) != "last" {
			t.Fatalf("got %q, want the final frame", got)
		}
		waitDone(t, c)
		if code, _ := tr.closeInfo(); code != rpcwire.ClosePolicyViolation {
			t.Fatalf("close %d, want 1008", code)
		}
	})
}

// A failing ping ends the conn within PingInterval + WriteTimeout.
func TestConnPingFailureCloses(t *testing.T) {
	tr := newMem(4)
	tr.pingErr.Store(errors.New("no pong"))
	c := run(t, tr, ConnOptions{SendQueue: 4, PingInterval: 50 * time.Millisecond, WriteTimeout: 100 * time.Millisecond}, func(*Conn, []byte) {})
	waitDone(t, c)
}

// keys is a one-key tenant.KeyResolver.
type keys struct {
	hash           []byte
	keyID, project uuid.UUID
}

func (k keys) ResolveKey(_ context.Context, h []byte) (uuid.UUID, uuid.UUID, error) {
	if string(h) != string(k.hash) {
		return uuid.Nil, uuid.Nil, tenant.ErrUnknownKey
	}
	return k.keyID, k.project, nil
}

type recorder struct {
	mu sync.Mutex
	ev []tenant.Event
}

func (r *recorder) Audit(e tenant.Event) { r.mu.Lock(); r.ev = append(r.ev, e); r.mu.Unlock() }

func (r *recorder) events() []tenant.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]tenant.Event(nil), r.ev...)
}

// A key-mode open over Cache.Session (the key as bearer metadata) is one session and one
// session_open event; the end of the stream removes it and records session_close; no event carries
// the key, its hash or the password.
func TestHandlerSessionAudit(t *testing.T) {
	const apiKey, password = "rlk_secret-key-material", "pw-secret"
	project := uuid.New()
	k := keys{hash: tenant.HashKey(apiKey), keyID: uuid.New(), project: project}
	reg := sheet.NewRegistry(sheet.Options{BcryptCost: 4})
	if _, err := reg.Create(project, "s1", password); err != nil {
		t.Fatal(err)
	}
	sessions := session.NewRegistry("node-1")
	rec := &recorder{}
	h := NewHandler(reg, lock.NewManager(nil), leader{}, tenant.New(k, true), sessions, rec, Options{SendQueue: 8, MaxFrameBytes: 1 << 20, WriteTimeout: time.Second})
	b := benchFor(t, h)

	ctx, cancel := context.WithCancel(metadata.AppendToOutgoingContext(context.Background(), rpcwire.AuthHeader, "Bearer "+apiKey))
	s := b.stream(t, ctx)
	s.send(&pb.ClientMsg{Id: 1, Body: &pb.ClientMsg_Open{Open: &pb.Open{Sheet: "s1", Password: password}}})
	if r := s.reply(); !r.GetSuccess() {
		t.Fatalf("open: %v", r)
	}
	s.state()
	l := sessions.List(session.Filter{})
	if len(l) != 1 || l[0].KeyID != k.keyID || l[0].Project != project || l[0].Node != "node-1" {
		t.Fatalf("sessions = %+v", l)
	}
	cancel()
	deadline := time.Now().Add(5 * time.Second)
	for sessions.Len() != 0 || len(rec.events()) < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("after close: %d sessions, events %+v", sessions.Len(), rec.events())
		}
		time.Sleep(10 * time.Millisecond)
	}
	ev := rec.events()
	if ev[0].Action != "session_open" || ev[1].Action != "session_close" {
		t.Fatalf("events = %+v", ev)
	}
	for _, e := range ev {
		if e.Project != project || e.Subject != l[0].ID.String() || e.Detail["key_id"] != k.keyID.String() || e.Detail["sheet"] != "s1" || e.Detail["remote"] == "" {
			t.Fatalf("event = %+v", e)
		}
		all := e.Actor + e.Subject
		for _, v := range e.Detail {
			all += v
		}
		if strings.Contains(all, apiKey) || strings.Contains(all, password) || strings.Contains(all, string(k.hash)) {
			t.Fatalf("event carries a secret: %+v", e)
		}
	}
}
