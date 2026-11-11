package rpc

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/CrimsonCoder42/red_lite_cache/internal/lock"
	"github.com/CrimsonCoder42/red_lite_cache/internal/logger"
	"github.com/CrimsonCoder42/red_lite_cache/internal/session"
	"github.com/CrimsonCoder42/red_lite_cache/internal/sheet"
	"github.com/CrimsonCoder42/red_lite_cache/internal/tenant"
)

// Node is this instance's place in the leader/replica pair. The router asks it before every open
// and write. Wave 2 ships a static node (internal/app); internal/replica implements the real one.
type Node interface {
	IsLeader() bool
	Epoch() uint64
	// ObserveEpoch is called when a client request carries an epoch higher than Epoch(): another
	// leader exists. The replica layer steps down on it.
	ObserveEpoch(e uint64)
}

// Options configures a Handler.
type Options struct {
	SendQueue     int           // REDLITE_CONN_SEND_QUEUE
	MaxFrameBytes int64         // REDLITE_MAX_FRAME_BYTES; a bigger frame closes the socket with 1009
	PingInterval  time.Duration // REDLITE_PING_INTERVAL
	WriteTimeout  time.Duration // REDLITE_WRITE_TIMEOUT
	LockTTL       time.Duration // REDLITE_LOCK_TTL: a lock with lock_ttl 0/absent
}

// Handler serves client sessions over a Transport (the gRPC Cache service, grpc.go): one Conn and
// one read loop per session,
// every frame routed by its action (router.go). It tracks live connections so CloseAll can kick
// them.
//
// after advo_cache/ws/handler.go:76-134 (Handler, ServeWS): the replica map and leader flag moved
// out (Node, internal/replica), sync.Map of clients became a map + Mutex of Conns, and the socket
// became a Transport (D-048).
type Handler struct {
	reg      *sheet.Registry
	locks    *lock.Manager
	node     Node
	tenants  *tenant.Directory // admission on open (D-024, D-025, D-027)
	sessions *session.Registry // one entry per admitted connection (D-026)
	audit    tenant.Auditor    // session_open / session_close
	opts     Options

	mu     sync.Mutex
	conns  map[uint64]*Conn
	closed bool
	reason string
	wg     sync.WaitGroup

	panics atomic.Uint64 // panics recovered on client connections (ConnOptions.Panics)
}

// NewHandler builds the session handler behind the Cache service. tenants decides every open (nil = password mode, nothing
// suspended); every admitted connection is a session in sessions (nil = a private registry) and its
// open and close are recorded through audit (nil = tenant.NopAuditor).
func NewHandler(reg *sheet.Registry, locks *lock.Manager, node Node, tenants *tenant.Directory,
	sessions *session.Registry, audit tenant.Auditor, opts Options) *Handler {
	if tenants == nil {
		tenants = tenant.New(nil, false)
	}
	if sessions == nil {
		sessions = session.NewRegistry("")
	}
	if audit == nil {
		audit = tenant.NopAuditor{}
	}
	return &Handler{reg: reg, locks: locks, node: node, tenants: tenants, sessions: sessions, audit: audit,
		opts: opts, conns: make(map[uint64]*Conn)}
}

// Serve runs one client session on t until it ends. ctx bounds admission's key lookup and the
// session (its end aborts the transport). After CloseAll the session is refused at once.
func (h *Handler) Serve(ctx context.Context, t Transport, remote string) {
	c := NewConn(t, ConnOptions{
		SendQueue: h.opts.SendQueue, PingInterval: h.opts.PingInterval,
		WriteTimeout: h.opts.WriteTimeout, Remote: remote, Panics: &h.panics,
	})
	if !h.track(c) {
		t.Close(1001, clip(h.reason))
		t.Abort()
		return
	}
	defer h.untrack(c)
	s := &client{ctx: ctx, h: h, c: c}
	c.log.Debug("RPC: connected", "remote", remote)
	err := c.Run(ctx, s.handle)
	s.end(err)
	c.log.Debug("RPC: disconnected", "remote", remote, "status", closeStatus(err))
}

// CloseError is a transport read error that carries the close code the connection ended with
// (the code this side closed the stream with).
type CloseError struct {
	Code int
	Err  error
}

func (e *CloseError) Error() string { return e.Err.Error() }
func (e *CloseError) Unwrap() error { return e.Err }

// closeStatus is the close code err carries, -1 when the connection ended without one.
func closeStatus(err error) int {
	if ce, ok := errors.AsType[*CloseError](err); ok {
		return ce.Code
	}
	return -1
}

func (h *Handler) track(c *Conn) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return false
	}
	h.conns[c.ID()] = c
	h.wg.Add(1)
	return true
}

func (h *Handler) untrack(c *Conn) {
	h.mu.Lock()
	delete(h.conns, c.ID())
	h.mu.Unlock()
	h.wg.Done()
}

// Panics is how many panics were recovered on client connections since the handler was built (a
// count, D-040). Nonzero means a bug that only a log line would otherwise show.
func (h *Handler) Panics() uint64 { return h.panics.Load() }

// Len is the number of live connections.
func (h *Handler) Len() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.conns)
}

// QueuedFrames is the sum of Queued over the live connections (a count, for leak checks, D-038).
func (h *Handler) QueuedFrames() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, c := range h.conns {
		n += c.Queued()
	}
	return n
}

// CloseAll kicks every live connection with reason and refuses new ones from then on (shutdown).
func (h *Handler) CloseAll(reason string) {
	h.mu.Lock()
	h.closed = true
	h.reason = reason
	conns := make([]*Conn, 0, len(h.conns))
	for _, c := range h.conns {
		conns = append(conns, c)
	}
	h.mu.Unlock()
	for _, c := range conns {
		c.Kick(reason)
	}
	logger.L.Info("RPC: closed all", "conns", len(conns), "reason", reason)
}

// KickAll kicks every live connection with reason but keeps accepting new ones (step-down: clients
// reconnect and are refused with "not leader").
func (h *Handler) KickAll(reason string) {
	h.mu.Lock()
	conns := make([]*Conn, 0, len(h.conns))
	for _, c := range h.conns {
		conns = append(conns, c)
	}
	h.mu.Unlock()
	for _, c := range conns {
		c.Kick(reason)
	}
	logger.L.Info("RPC: kicked all", "conns", len(conns), "reason", reason)
}

// Wait blocks until every connection's Serve has returned, or ctx is done; it then returns
// ctx.Err(). Call it after CloseAll.
func (h *Handler) Wait(ctx context.Context) error {
	t := time.NewTicker(10 * time.Millisecond)
	defer t.Stop()
	for {
		if h.Len() == 0 {
			h.wg.Wait() // every untrack has run; returns at once
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}
