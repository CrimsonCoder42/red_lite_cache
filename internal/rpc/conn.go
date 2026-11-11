package rpc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/CrimsonCoder42/red_lite_cache/internal/logger"
	"github.com/CrimsonCoder42/red_lite_cache/internal/session"
	"github.com/CrimsonCoder42/red_lite_cache/protocol/rpcwire"
)

// Transport is one client or replica connection's wire: a gRPC stream (StreamTransport, grpc.go).
// Frames are the JSON frames of package protocol; the gRPC transport
// translates them to and from the .proto messages (protocol/rpcwire, D-048).
//
// Concurrency: Recv is called only by Conn's read loop, Write and Ping only by its writer
// goroutine, Close and Abort from any goroutine (idempotent, non-blocking).
type Transport interface {
	// Recv returns the next inbound frame (a JSON request). It returns an error once the
	// connection is over: peer gone, Close or Abort called, or a protocol error.
	Recv() ([]byte, error)
	// Write sends one outbound frame. It may block on the peer's flow control; Abort unblocks it.
	Write(ctx context.Context, frame []byte) error
	// Close ends the connection with a v1 close code and reason (rpcwire.CloseStatus maps them to
	// a gRPC status). Frames already written are delivered first.
	Close(code int, reason string)
	// Abort ends the connection at once, unblocking a Write stuck on a slow peer.
	Abort()
	// Ping checks the peer is alive; nil when the transport does it itself (gRPC keepalive).
	Ping(ctx context.Context) error
}

// connSeq hands out process-unique connection ids (lock owner, sheet collaborator key, logs).
var connSeq atomic.Uint64

// ConnOptions configures one Conn.
type ConnOptions struct {
	SendQueue    int           // frames queued before Send returns false (D-014); < 1 = 1
	PingInterval time.Duration // keepalive ping period; 0 = no ping loop
	WriteTimeout time.Duration // deadline for one frame write or one ping round trip; 0 = 10s
	Remote       string        // peer address, for logs
	// Panics, when set, is incremented once per panic guard recovers (D-040: a count only). The
	// Handler passes its own, so /health and the test harness see recovered panics, not just a log.
	Panics *atomic.Uint64
}

// outFrame is one queued frame. closeAfter makes the writer close the connection right after
// writing it (SendAndClose).
type outFrame struct {
	data       []byte
	closeAfter bool
	code       int
	reason     string
}

// Conn is one connection with a bounded send queue drained by ONE writer goroutine, and a ping
// loop. Every frame to the peer goes through Send, so nothing but the writer ever writes to the
// transport (K9; a gRPC stream's SendMsg is not safe from several goroutines, grpc-go stream.go)
// and a slow peer only fills its own queue: a full queue kicks it (K15, D-014) and Abort unblocks
// a write held by the peer's flow control, so a broadcast never waits on a reader. It implements
// sheet.Collaborator; internal/replica uses it for the leader → replica stream.
//
// after internal/ws.Conn (2026-10-29), itself after advo_cache/ws/handler.go:104-134: the socket
// became a Transport (gRPC: StreamTransport) behind one queue, writer and close path (D-048).
type Conn struct {
	id   uint64
	t    Transport
	opts ConnOptions
	log  *slog.Logger

	send    chan outFrame
	final   atomic.Bool // SendAndClose queued: every later Send is refused
	kick    chan struct{}
	once    sync.Once
	code    int // set once, before kick is closed
	reason  string
	done    chan struct{}
	running atomic.Bool

	// sess is the admitted session (D-026), set once by the router on open. The reader touches it
	// per inbound frame and the writer counts per written frame: atomics only, no lock.
	sess atomic.Pointer[session.Session]
}

// NewConn wraps a transport. Run starts the writer and ping loop.
func NewConn(t Transport, opts ConnOptions) *Conn {
	opts.SendQueue = max(opts.SendQueue, 1)
	if opts.WriteTimeout <= 0 {
		opts.WriteTimeout = 10 * time.Second
	}
	id := connSeq.Add(1)
	return &Conn{
		id:   id,
		t:    t,
		opts: opts,
		log:  logger.L.With("conn_id", id),
		send: make(chan outFrame, opts.SendQueue),
		kick: make(chan struct{}),
		done: make(chan struct{}),
	}
}

// ID is unique per connection for the life of the process.
func (c *Conn) ID() uint64 { return c.id }

// Remote is the peer address given in ConnOptions.
func (c *Conn) Remote() string { return c.opts.Remote }

// Queued is the number of frames waiting in the send queue (a count, for leak checks, D-038).
func (c *Conn) Queued() int { return len(c.send) }

// Done is closed when Run has returned and the transport is closed.
func (c *Conn) Done() <-chan struct{} { return c.done }

// Log is the connection's logger (conn_id attached).
func (c *Conn) Log() *slog.Logger { return c.log }

// Send enqueues frame without blocking. It returns false when the queue is full or the connection
// is closing; the caller then Kicks (the sheet does, D-014).
func (c *Conn) Send(frame []byte) bool {
	if c.final.Load() || c.kicked() {
		return false
	}
	select {
	case c.send <- outFrame{data: frame}:
		return true
	default:
		return false
	}
}

// SendAndClose enqueues frame and closes the connection with code and reason once it is written,
// so the peer reads the reply before the close (invalid password). A full queue kicks instead.
func (c *Conn) SendAndClose(frame []byte, code int, reason string) {
	if !c.final.CompareAndSwap(false, true) {
		return
	}
	select {
	case c.send <- outFrame{data: frame, closeAfter: true, code: code, reason: clip(reason)}:
	default:
		c.closeWith(code, reason)
	}
}

// Kick closes the connection with going-away (1001) and reason (CloseWith). The peer resyncs on
// reconnect.
func (c *Conn) Kick(reason string) { c.CloseWith(rpcwire.CloseGoingAway, reason) }

// CloseWith closes the connection with code and reason without blocking: the writer ends the
// connection with that code (a reader sees 4001/4002/4003, D-030) and frames still queued are
// dropped; a write held by the peer's flow control is aborted. Idempotent (the first close wins);
// safe under the sheet and session-registry locks; it is the session's Kill func. After
// SendAndClose it does nothing: the close is already queued behind the final frame.
func (c *Conn) CloseWith(code int, reason string) {
	if c.final.Load() {
		return
	}
	c.closeWith(code, reason)
}

// Session is the session this connection was admitted as, nil before a successful open.
func (c *Conn) Session() *session.Session { return c.sess.Load() }

// Bind records the admitted session.
func (c *Conn) Bind(s *session.Session) { c.sess.Store(s) }

func (c *Conn) closeWith(code int, reason string) {
	c.once.Do(func() {
		c.code, c.reason = code, clip(reason)
		close(c.kick)
		// The writer may be inside Write, held by a peer that stopped reading: set the close
		// status first, then abort, so the stream ends with this code and the writer unblocks.
		c.t.Close(c.code, c.reason)
		c.t.Abort()
	})
}

func (c *Conn) kicked() bool {
	select {
	case <-c.kick:
		return true
	default:
		return false
	}
}

// clip keeps a close reason short (123 bytes, the v1 close-frame limit kept for log lines).
func clip(s string) string {
	if len(s) > 120 {
		return s[:120]
	}
	return s
}

// Run owns the connection until it ends: it starts the writer and the ping loop, then reads frames
// in the calling goroutine and passes each to handle, in order. It returns when the connection
// ends (peer gone, read error, Kick, a failed write or ping, or ctx done), after both goroutines
// have exited; Done is closed then. A panic in handle or in either goroutine is recovered, logged
// at Error, counted (ConnOptions.Panics), and closes only this connection. Run may be called once.
func (c *Conn) Run(ctx context.Context, handle func(frame []byte)) error {
	if !c.running.CompareAndSwap(false, true) {
		return errors.New("rpc: conn: Run called twice")
	}
	defer close(c.done)
	lctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Go(func() { c.guard("writer", func() { c.writeLoop(lctx) }) })
	if c.opts.PingInterval > 0 {
		wg.Go(func() { c.guard("ping", func() { c.pingLoop(lctx) }) })
	}
	// ctx done (shutdown) must end the read loop too: abort the transport.
	stop := context.AfterFunc(ctx, c.t.Abort)
	defer stop()
	var err error
	c.guard("read", func() { err = c.readLoop(handle) })
	cancel()
	c.t.Abort() // the writer may be blocked in Write: unblock it before waiting
	wg.Wait()
	return err
}

// guard runs fn and turns a panic into an Error log, one count in ConnOptions.Panics and a kick of
// this connection only.
func (c *Conn) guard(where string, fn func()) {
	defer func() {
		if p := recover(); p != nil {
			if c.opts.Panics != nil {
				c.opts.Panics.Add(1)
			}
			c.log.Error("RPC: panic recovered", "where", where, "panic", fmt.Sprint(p))
			c.Kick("internal error")
			c.t.Abort()
		}
	}()
	fn()
}

// readLoop reads until the transport fails.
func (c *Conn) readLoop(handle func(frame []byte)) error {
	for {
		data, err := c.t.Recv()
		if err != nil {
			return fmt.Errorf("rpc: conn %d: read: %w", c.id, err)
		}
		if s := c.sess.Load(); s != nil {
			s.Touch(time.Now())
		}
		handle(data)
	}
}

// writeLoop is the one goroutine that writes frames. It closes the transport on Kick (with the
// kick's code), after a closeAfter frame, on a failed write, and on ctx done (going away).
func (c *Conn) writeLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			if !c.kicked() {
				c.t.Close(rpcwire.CloseGoingAway, "connection closing")
			}
			c.t.Abort()
			return
		case <-c.kick:
			c.t.Abort() // closeWith already set the code
			return
		case f := <-c.send:
			if err := c.write(ctx, f.data); err != nil {
				c.log.Debug("RPC: write failed", "err", err)
				c.t.Abort()
				return
			}
			if s := c.sess.Load(); s != nil {
				s.Sent()
			}
			if f.closeAfter {
				c.t.Close(f.code, f.reason)
				c.t.Abort()
				return
			}
		}
	}
}

// write sends one frame within WriteTimeout. A transport that cannot cancel a blocked write by
// context (a gRPC stream held by flow control) is aborted when the deadline passes.
func (c *Conn) write(ctx context.Context, b []byte) error {
	wctx, cancel := context.WithTimeout(ctx, c.opts.WriteTimeout)
	defer cancel()
	stop := context.AfterFunc(wctx, func() {
		if errors.Is(wctx.Err(), context.DeadlineExceeded) {
			c.t.Abort()
		}
	})
	defer stop()
	return c.t.Write(wctx, b)
}

// pingLoop pings every PingInterval; a ping without an answer inside WriteTimeout ends the
// connection. A transport with its own keepalive returns nil at once.
func (c *Conn) pingLoop(ctx context.Context) {
	t := time.NewTicker(c.opts.PingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pctx, cancel := context.WithTimeout(ctx, c.opts.WriteTimeout)
			err := c.t.Ping(pctx)
			cancel()
			if err != nil {
				if ctx.Err() == nil && !c.kicked() {
					c.log.Warn("RPC: ping failed", "remote", c.opts.Remote, "err", err)
				}
				c.t.Abort()
				return
			}
		}
	}
}
