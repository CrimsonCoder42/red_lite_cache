package ws

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/CrimsonCoder42/red_lite_cache/internal/logger"
	"github.com/CrimsonCoder42/red_lite_cache/internal/session"
)

// connSeq hands out process-unique connection ids (lock owner, sheet collaborator key, logs).
var connSeq atomic.Uint64

// ConnOptions configures one Conn.
type ConnOptions struct {
	SendQueue    int           // frames queued before Send returns false (D-014); < 1 = 1
	PingInterval time.Duration // keepalive ping period; 0 = no ping loop
	WriteTimeout time.Duration // deadline for one frame write or one ping round trip; 0 = 10s
	Remote       string        // peer address, for logs
}

// outFrame is one queued frame. closeAfter makes the writer close the socket right after writing it
// (SendAndClose).
type outFrame struct {
	data       []byte
	closeAfter bool
	code       websocket.StatusCode
	reason     string
}

// Conn is one WebSocket connection with a bounded send queue drained by ONE writer goroutine, and a
// ping loop. Every frame to the peer goes through Send, so nothing but the writer ever writes to the
// socket (K9) and a slow peer only fills its own queue (K15, D-014). It implements
// sheet.Collaborator; internal/replica reuses it for the leader → replica stream.
//
// after advo_cache/ws/handler.go:104-134 (ServeWS): a gorilla conn written directly from every
// goroutine that broadcast to it became a coder/websocket conn (D-011) owned by one writer, after
// code-heim go_75_coder_websocket (doc/reference/codeheim_patterns.md § Tier 2) plus a bounded
// queue, a per-frame write deadline and a non-blocking Kick.
type Conn struct {
	id   uint64
	ws   *websocket.Conn
	opts ConnOptions
	log  *slog.Logger

	send    chan outFrame
	final   atomic.Bool // SendAndClose queued: every later Send is refused
	kick    chan struct{}
	once    sync.Once
	code    websocket.StatusCode // set once, before kick is closed
	reason  string
	done    chan struct{}
	running atomic.Bool

	// sess is the admitted session (D-026), set once by the router on open. The reader touches it
	// per inbound frame and the writer counts per written frame: atomics only, no lock.
	sess atomic.Pointer[session.Session]
}

// NewConn wraps an accepted or dialled socket. The caller sets the read limit on ws; Run starts
// the writer and ping loop.
func NewConn(ws *websocket.Conn, opts ConnOptions) *Conn {
	opts.SendQueue = max(opts.SendQueue, 1)
	if opts.WriteTimeout <= 0 {
		opts.WriteTimeout = 10 * time.Second
	}
	id := connSeq.Add(1)
	return &Conn{
		id:   id,
		ws:   ws,
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

// Done is closed when Run has returned and the socket is closed.
func (c *Conn) Done() <-chan struct{} { return c.done }

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

// SendAndClose enqueues frame and closes the socket with code and reason once it is written, so the
// peer reads the reply before the close (invalid password). A full queue kicks instead.
func (c *Conn) SendAndClose(frame []byte, code websocket.StatusCode, reason string) {
	if !c.final.CompareAndSwap(false, true) {
		return
	}
	select {
	case c.send <- outFrame{data: frame, closeAfter: true, code: code, reason: clip(reason)}:
	default:
		c.closeWith(code, reason)
	}
}

// Kick closes the connection with StatusGoingAway and reason (CloseWith). The peer resyncs on
// reconnect.
func (c *Conn) Kick(reason string) { c.CloseWith(websocket.StatusGoingAway, reason) }

// CloseWith closes the connection with code and reason without blocking: the writer sends that
// close frame to the peer (a reader sees 4001/4002/4003, D-030) and frames still queued are
// dropped. Idempotent (the first close wins); safe under the sheet and session-registry locks; it
// is the session's Kill func. After SendAndClose it does nothing: the close is already queued
// behind the final frame, and closing now would let the writer drop that frame (a step-down's
// KickAll or an admin kill racing a stale-epoch or refusal reply).
func (c *Conn) CloseWith(code websocket.StatusCode, reason string) {
	if c.final.Load() {
		return
	}
	c.closeWith(code, reason)
}

// Session is the session this connection was admitted as, nil before a successful open.
func (c *Conn) Session() *session.Session { return c.sess.Load() }

func (c *Conn) bind(s *session.Session) { c.sess.Store(s) }

func (c *Conn) closeWith(code websocket.StatusCode, reason string) {
	c.once.Do(func() {
		c.code, c.reason = code, clip(reason)
		close(c.kick)
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

// clip keeps a close reason within the 123 bytes a close frame allows.
func clip(s string) string {
	if len(s) > 120 {
		return s[:120]
	}
	return s
}

// Run owns the connection until it ends: it starts the writer and the ping loop, then reads frames
// in the calling goroutine and passes each to handle, in order. It returns when the socket closes
// (peer close, read error, Kick, a failed write or ping, or ctx done), after both goroutines have
// exited and the socket is closed; Done is closed then. A panic in handle or in either goroutine is
// recovered, logged at Error, and closes only this socket. Run may be called once.
func (c *Conn) Run(ctx context.Context, handle func(frame []byte)) error {
	if !c.running.CompareAndSwap(false, true) {
		return errors.New("ws: conn: Run called twice")
	}
	defer close(c.done)
	lctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Go(func() { c.guard("writer", func() { c.writeLoop(lctx) }) })
	if c.opts.PingInterval > 0 {
		wg.Go(func() { c.guard("ping", func() { c.pingLoop(lctx) }) })
	}
	var err error
	c.guard("read", func() { err = c.readLoop(handle) })
	cancel()
	wg.Wait()
	c.ws.CloseNow()
	return err
}

// guard runs fn and turns a panic into an Error log and a kick of this connection only.
func (c *Conn) guard(where string, fn func()) {
	defer func() {
		if p := recover(); p != nil {
			c.log.Error("WS: panic recovered", "where", where, "panic", fmt.Sprint(p))
			c.Kick("internal error")
			c.ws.CloseNow()
		}
	}()
	fn()
}

// readLoop reads until the socket fails. It reads under a context that only the socket's closing
// ends: coder/websocket closes the socket when a read's context ends, and the writer owns the close.
func (c *Conn) readLoop(handle func(frame []byte)) error {
	for {
		_, data, err := c.ws.Read(context.Background())
		if err != nil {
			return fmt.Errorf("ws: conn %d: read: %w", c.id, err)
		}
		if s := c.sess.Load(); s != nil {
			s.Touch(time.Now())
		}
		handle(data)
	}
}

// writeLoop is the one goroutine that writes frames. It closes the socket on Kick (with the kick's
// code), after a closeAfter frame, on a failed write, and on ctx done (StatusGoingAway).
func (c *Conn) writeLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			if c.kicked() {
				c.ws.Close(c.code, c.reason)
				return
			}
			c.ws.Close(websocket.StatusGoingAway, "connection closing")
			return
		case <-c.kick:
			c.ws.Close(c.code, c.reason)
			return
		case f := <-c.send:
			if err := c.write(ctx, f.data); err != nil {
				c.log.Debug("WS: write failed", "err", err)
				c.ws.CloseNow()
				return
			}
			if s := c.sess.Load(); s != nil {
				s.Sent()
			}
			if f.closeAfter {
				c.ws.Close(f.code, f.reason)
				return
			}
		}
	}
}

func (c *Conn) write(ctx context.Context, b []byte) error {
	wctx, cancel := context.WithTimeout(ctx, c.opts.WriteTimeout)
	defer cancel()
	return c.ws.Write(wctx, websocket.MessageText, b)
}

// pingLoop pings every PingInterval; a ping without a pong inside WriteTimeout closes the socket.
// The pong is read by readLoop (coder/websocket handles control frames inside Read).
func (c *Conn) pingLoop(ctx context.Context) {
	t := time.NewTicker(c.opts.PingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pctx, cancel := context.WithTimeout(ctx, c.opts.WriteTimeout)
			err := c.ws.Ping(pctx)
			cancel()
			if err != nil {
				if ctx.Err() == nil && !c.kicked() && !errors.Is(err, net.ErrClosed) {
					c.log.Warn("WS: ping failed", "remote", c.opts.Remote, "err", err)
				}
				c.ws.CloseNow()
				return
			}
		}
	}
}
