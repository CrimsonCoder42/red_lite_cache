package ws

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

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

// Handler serves client WebSockets on protocol.PathWS: one Conn and one read loop per socket, every
// frame routed by its action (router.go). It tracks live connections so CloseAll can kick them.
//
// after advo_cache/ws/handler.go:76-134 (Handler, ServeWS): the replica map and leader flag moved
// out (Node, internal/replica), sync.Map of clients became a map + Mutex of Conns, and the
// allow-all-origins upgrader became websocket.Accept's default origin policy.
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
}

// NewHandler builds the /ws handler. tenants decides every open (nil = password mode, nothing
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

// ServeHTTP upgrades the request and serves the socket until it closes.
//
// Origin: websocket.Accept's default policy accepts a request with no Origin header (every backend
// client: Go's dialer sends none) and one whose Origin host equals the request Host; a browser page
// from another origin is refused with 403. No OriginPatterns are configured.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	wsc, err := websocket.Accept(w, r, nil)
	if err != nil {
		logger.L.Warn("WS: upgrade failed", "remote", r.RemoteAddr, "err", err)
		return
	}
	wsc.SetReadLimit(h.opts.MaxFrameBytes)
	c := NewConn(wsc, ConnOptions{
		SendQueue: h.opts.SendQueue, PingInterval: h.opts.PingInterval,
		WriteTimeout: h.opts.WriteTimeout, Remote: r.RemoteAddr,
	})
	if !h.track(c) {
		wsc.Close(websocket.StatusGoingAway, clip(h.reason))
		return
	}
	defer h.untrack(c)
	s := &client{ctx: r.Context(), h: h, c: c}
	c.log.Debug("WS: connected", "remote", r.RemoteAddr)
	err = c.Run(r.Context(), s.handle)
	s.end(err)
	c.log.Debug("WS: disconnected", "remote", r.RemoteAddr, "status", int(websocket.CloseStatus(err)))
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
	logger.L.Info("WS: closed all", "conns", len(conns), "reason", reason)
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
	logger.L.Info("WS: kicked all", "conns", len(conns), "reason", reason)
}

// Wait blocks until every connection's ServeHTTP has returned, or ctx is done; it then returns
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
