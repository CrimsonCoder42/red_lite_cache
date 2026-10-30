package replica

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/CrimsonCoder42/red_lite_cache/db"
	"github.com/CrimsonCoder42/red_lite_cache/internal/logger"
	"github.com/CrimsonCoder42/red_lite_cache/internal/persist"
	"github.com/CrimsonCoder42/red_lite_cache/internal/sheet"
	"github.com/CrimsonCoder42/red_lite_cache/protocol"
)

// Roles, as /health reports them.
const (
	RoleLeader  = "leader"
	RoleReplica = "replica"
)

// Options configures a Node. Every duration comes from internal/config.
type Options struct {
	Role       string // RoleLeader | RoleReplica at boot (REDLITE_ROLE)
	LeaderAddr string // replica: the leader, ws://host:port[/replica] (REDLITE_LEADER_ADDRESS)
	PeerAddr   string // the other node, ws://host:port (REDLITE_PEER_ADDRESS); "" = no probe
	// ClusterToken is REDLITE_CLUSTER_TOKEN (D-031): required as a Bearer token on /replica and the
	// peer /health probe, and sent by this node when it dials or probes. "" = no peer auth (dev only).
	ClusterToken string
	InstanceID   string

	HeartbeatInterval time.Duration // REDLITE_HEARTBEAT_INTERVAL: leader → replica heartbeat, peer probe
	HeartbeatTimeout  time.Duration // REDLITE_HEARTBEAT_TIMEOUT: replica promotes after this long without a frame

	SendQueue     int           // REDLITE_REPLICA_SEND_QUEUE: frames queued per replica connection
	MaxFrameBytes int64         // REDLITE_REPLICA_MAX_FRAME_BYTES: read limit on both ends of the stream
	PingInterval  time.Duration // REDLITE_PING_INTERVAL
	WriteTimeout  time.Duration // REDLITE_WRITE_TIMEOUT

	// Store persists the epoch (cluster_state) when persistence is on; nil = epoch in memory only.
	Store *db.Store
	// KickClients kicks every client connection on /ws (ws.Handler.KickAll) at step-down.
	KickClients func(reason string)
	// OnPromote runs after this node became leader by promotion, before it serves as leader.
	// synced is false when no leader snapshot ever reached this node since boot.
	OnPromote func(ctx context.Context, synced bool)
	// OnControl is called on a replica for each tenancy control frame from the leader
	// (project_suspended, project_resumed, key_revoked, session_kill — D-027/D-028/D-030), used when
	// persistence is off and NOTIFY cannot carry them. Must not block. nil = ignored.
	OnControl func(protocol.ReplicaControl)
	// Elector is REDLITE_ELECTION=advisory (D-022; persist.Elector): nil = epoch mode (D-012) only.
	// With it a node leads only while it holds the lock — a boot leader starts as a replica and
	// promotes once it has the lock, a replica promotes on timeout only if it gets the lock, and a
	// leader whose lock connection fails steps down at once. The epoch still fences.
	Elector Elector
}

// Elector is the advisory-lock leader election (implemented by persist.Elector).
type Elector interface {
	TryAcquire(ctx context.Context) bool
	Release(ctx context.Context)
	Run(ctx context.Context, interval time.Duration, onLost func()) error
}

// Node is this instance's place in the pair. All state is atomic or under mu; no lock is held
// across network IO.
//
// after advo_cache/replica/client.go:16-37 (Client) and ws/handler.go:453-632 (the leader side):
// the two halves became one node with a role that can move both ways, a fencing epoch (D-012),
// a bounded queue per replica connection (ws.Conn) instead of direct writes, and every loop on a ctx.
type Node struct {
	reg  *sheet.Registry
	opts Options

	leader    atomic.Bool
	epoch     atomic.Uint64
	startedAt time.Time

	lastFrame atomic.Int64 // unix nanos of the last frame from a leader (or boot / step-down)
	synced    atomic.Bool  // a snapshot_end was applied since boot

	stepCh   chan struct{} // a step-down happened; the Run loop leaves the leader state
	probeNow chan struct{} // checkPeer: run the peer probe without waiting for the next tick

	hub hub
}

// New builds the node and sets its boot epoch: a leader with persistence resumes the stored epoch
// (0 → 1, stored), a leader without starts at 1, a replica at 0 until its leader tells it.
func New(ctx context.Context, reg *sheet.Registry, o Options) (*Node, error) {
	if o.Role != RoleLeader && o.Role != RoleReplica {
		return nil, fmt.Errorf("replica: role %q: must be leader or replica", o.Role)
	}
	if o.HeartbeatInterval <= 0 || o.HeartbeatTimeout <= o.HeartbeatInterval {
		return nil, errors.New("replica: heartbeat timeout must exceed a positive heartbeat interval")
	}
	if o.KickClients == nil {
		o.KickClients = func(string) {}
	}
	n := &Node{reg: reg, opts: o, startedAt: time.Now(), stepCh: make(chan struct{}, 1), probeNow: make(chan struct{}, 1)}
	n.hub.conns = map[uint64]*replicaConn{}
	n.touch()
	if o.Role == RoleReplica {
		return n, nil
	}
	if o.Elector != nil {
		if o.Store == nil {
			return nil, errors.New("replica: advisory election requires persistence")
		}
		// Not leader until Run holds the lock; promote then raises the stored epoch by one.
		stored, err := persist.LoadEpoch(ctx, o.Store)
		if err != nil {
			return nil, fmt.Errorf("replica: boot epoch: %w", err)
		}
		n.epoch.Store(stored)
		return n, nil
	}
	n.leader.Store(true)
	epoch := uint64(1)
	if o.Store != nil {
		stored, err := persist.LoadEpoch(ctx, o.Store)
		if err != nil {
			return nil, fmt.Errorf("replica: boot epoch: %w", err)
		}
		if stored > 0 {
			epoch = stored
		} else if epoch, err = persist.RaiseEpoch(ctx, o.Store, 1, o.InstanceID); err != nil {
			return nil, fmt.Errorf("replica: boot epoch: %w", err)
		}
	}
	n.epoch.Store(epoch)
	return n, nil
}

// IsLeader reports whether this node accepts client opens and writes now.
func (n *Node) IsLeader() bool { return n.leader.Load() }

// Epoch is the highest leader epoch this node knows.
func (n *Node) Epoch() uint64 { return n.epoch.Load() }

// Role is RoleLeader or RoleReplica.
func (n *Node) Role() string {
	if n.IsLeader() {
		return RoleLeader
	}
	return RoleReplica
}

// Replicas is the number of registered replica connections (leader side).
func (n *Node) Replicas() int { return n.hub.len() }

// ObserveEpoch is called by the /ws router when a client request carries an epoch above ours. The
// request is refused (stale epoch) but the claim is not trusted — an unauthenticated frame must not
// be able to fence a leader — so it only makes the peer probe check now. Without
// REDLITE_PEER_ADDRESS nothing changes here; readers that saw the higher epoch still refuse this
// node (D-012), so there is no split-brain on the read side.
func (n *Node) ObserveEpoch(e uint64) {
	if e > n.Epoch() && n.IsLeader() {
		logger.L.Debug("Replica: higher epoch claimed", "epoch", n.Epoch(), "claimed", e)
		n.checkPeer()
	}
}

// checkPeer asks the peer probe to run now (non-blocking; one pending signal at most, so a flood
// of claims costs at most one probe per probe round trip).
func (n *Node) checkPeer() {
	select {
	case n.probeNow <- struct{}{}:
	default:
	}
}

func (n *Node) touch() { n.lastFrame.Store(time.Now().UnixNano()) }

func (n *Node) sinceFrame() time.Duration {
	return time.Since(time.Unix(0, n.lastFrame.Load()))
}

// adopt raises the known epoch to e (never lowers it).
func (n *Node) adopt(e uint64) {
	for {
		cur := n.epoch.Load()
		if e <= cur || n.epoch.CompareAndSwap(cur, e) {
			return
		}
	}
}

// followTarget is whom a stepped-down leader follows: the peer, else the boot leader address.
func (n *Node) followTarget() string {
	if n.opts.PeerAddr != "" {
		return n.opts.PeerAddr
	}
	return n.opts.LeaderAddr
}

// stepDown fences this leader: it stops accepting writes at once, kicks every client and replica
// (readers fail over), and tells Run to follow the peer. Safe from any goroutine; only the first
// call of a leader term acts.
func (n *Node) stepDown(newEpoch uint64, reason string) {
	if !n.leader.CompareAndSwap(true, false) {
		return
	}
	old := n.Epoch()
	n.adopt(newEpoch)
	n.touch() // the heartbeat timeout of the new follower term starts now
	logger.L.Warn("Replica: stepping down", "epoch", old, "new_epoch", newEpoch, "reason", reason, "peer", n.followTarget())
	n.opts.KickClients("leader stepped down")
	n.reg.KickAll("leader stepped down")
	n.hub.closeAll("leader stepped down")
	select {
	case n.stepCh <- struct{}{}:
	default:
	}
}

// promote makes this replica the leader with epoch max(seen, stored)+1, stored first when
// persistence is on. A database error is logged and the node promotes on the in-memory epoch:
// availability over a durable epoch.
func (n *Node) promote(ctx context.Context) {
	next := n.Epoch() + 1
	if n.opts.Store != nil {
		if stored, err := persist.LoadEpoch(ctx, n.opts.Store); err != nil {
			logger.L.Error("Replica: epoch load failed", "err", err)
		} else if stored >= next {
			next = stored + 1
		}
		if got, err := persist.RaiseEpoch(ctx, n.opts.Store, next, n.opts.InstanceID); err != nil {
			logger.L.Error("Replica: epoch persist failed", "epoch", next, "err", err)
		} else if got > next {
			next = got + 1
			if _, err := persist.RaiseEpoch(ctx, n.opts.Store, next, n.opts.InstanceID); err != nil {
				logger.L.Error("Replica: epoch persist failed", "epoch", next, "err", err)
			}
		}
	}
	n.adopt(next)
	synced := n.synced.Load()
	if n.opts.OnPromote != nil {
		n.opts.OnPromote(ctx, synced)
	}
	n.leader.Store(true)
	logger.L.Info("Replica: promoted", "epoch", n.Epoch(), "synced", synced, "since_last_frame_ms", n.sinceFrame().Milliseconds())
}

// Run is the node's one entry point: it alternates between the leader state (heartbeats, peer
// probe) and the replica state (follow, apply, promote on timeout) until ctx ends, then closes
// every replica connection and waits for their handlers. Returns nil.
func (n *Node) Run(ctx context.Context) error {
	defer n.hub.shutdown(n.opts.WriteTimeout + time.Second)
	el := n.opts.Elector
	if el != nil {
		var wg sync.WaitGroup
		defer wg.Wait()
		ectx, cancel := context.WithCancel(ctx)
		defer cancel() // runs before wg.Wait (LIFO)
		wg.Go(func() {
			_ = el.Run(ectx, n.opts.HeartbeatInterval, func() { n.stepDown(n.Epoch(), "advisory lock lost") })
		})
		if n.opts.Role == RoleLeader && el.TryAcquire(ctx) {
			n.promote(ctx) // the boot leader: lock first, then epoch+1 (fences any older holder)
		}
	}
	for ctx.Err() == nil {
		if n.IsLeader() {
			n.runLeader(ctx)
			if el != nil && ctx.Err() == nil {
				el.Release(ctx) // stepped down (peer probe or lock lost): never hold the lock as a replica
			}
			continue
		}
		target := n.followTarget()
		if target == "" {
			if el == nil {
				logger.L.Error("Replica: fenced without peer", "epoch", n.Epoch())
				<-ctx.Done()
				return nil
			}
			n.awaitLock(ctx) // advisory, nobody to follow: lead as soon as the lock is free
			continue
		}
		if !n.follow(ctx, target) || ctx.Err() != nil {
			continue
		}
		if el != nil && !el.TryAcquire(ctx) {
			// The lock decides (D-022): another node holds it, so this one keeps following.
			logger.L.Warn("Replica: promotion deferred", "reason", "advisory lock held elsewhere", "epoch", n.Epoch())
			n.touch()
			continue
		}
		n.promote(ctx)
	}
	return nil
}

// awaitLock tries the advisory lock every heartbeat interval and promotes once it holds it.
func (n *Node) awaitLock(ctx context.Context) {
	t := time.NewTicker(n.opts.HeartbeatInterval)
	defer t.Stop()
	for {
		if n.opts.Elector.TryAcquire(ctx) {
			n.promote(ctx)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// replicaURL turns a leader address into its /replica URL: no path or "/" or "/ws" → /replica.
func replicaURL(addr string) (string, error) {
	u, err := url.Parse(addr)
	if err != nil {
		return "", fmt.Errorf("replica: leader address %q: %w", addr, err)
	}
	if p := strings.TrimSuffix(u.Path, "/"); p == "" || p == protocol.PathWS {
		u.Path = protocol.PathReplica
	}
	return u.String(), nil
}

// healthURL turns a peer address (ws:// or wss://, any path) into its http(s) /health URL.
func healthURL(addr string) (string, error) {
	u, err := url.Parse(addr)
	if err != nil {
		return "", fmt.Errorf("replica: peer address %q: %w", addr, err)
	}
	switch u.Scheme {
	case "ws":
		u.Scheme = "http"
	case "wss":
		u.Scheme = "https"
	}
	u.Path, u.RawQuery = protocol.PathHealth, ""
	return u.String(), nil
}

// hub is the leader's set of replica connections, keyed by connection id, with one connection per
// instance id. closed is set once by shutdown; after it no handler enters and no connection is
// added, so wg.Add never runs concurrently with shutdown's wg.Wait.
type hub struct {
	mu     sync.RWMutex
	conns  map[uint64]*replicaConn
	closed bool
	wg     sync.WaitGroup // one per ServeHTTP in flight
}

// enter counts one /replica handler in; false once shutdown started (the handler refuses).
func (h *hub) enter() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return false
	}
	h.wg.Add(1)
	return true
}

func (h *hub) leave() { h.wg.Done() }

func (h *hub) len() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.conns)
}

// add registers rc and returns the connection it replaced (same instance id), if any. ok is false
// after shutdown started: the caller kicks rc.
func (h *hub) add(rc *replicaConn) (old *replicaConn, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, false
	}
	for id, c := range h.conns {
		if c.instanceID == rc.instanceID {
			old = c
			delete(h.conns, id)
		}
	}
	h.conns[rc.c.ID()] = rc
	return old, true
}

func (h *hub) remove(rc *replicaConn) {
	h.mu.Lock()
	if cur, ok := h.conns[rc.c.ID()]; ok && cur == rc {
		delete(h.conns, rc.c.ID())
	}
	h.mu.Unlock()
}

// each runs fn for every registered connection under the read lock (fn must not block).
func (h *hub) each(fn func(*replicaConn)) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, rc := range h.conns {
		fn(rc)
	}
}

func (h *hub) closeAll(reason string) {
	h.mu.Lock()
	conns := h.conns
	h.conns = map[uint64]*replicaConn{}
	h.mu.Unlock()
	for _, rc := range conns {
		rc.c.Kick(reason)
	}
}

// shutdown closes the hub (no handler enters after this), kicks every replica connection and waits
// up to d for their handlers to return. The waiter goroutine is bounded: wg.Wait returns as soon
// as the kicked handlers return (each ends when its socket closes, itself bounded by the write
// timeout); when d passes first it outlives shutdown only until then, never past the process.
func (h *hub) shutdown(d time.Duration) {
	h.mu.Lock()
	h.closed = true
	h.mu.Unlock()
	h.closeAll("server shutting down")
	done := make(chan struct{})
	go func() { h.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
		logger.L.Warn("Replica: shutdown wait timed out", "replicas", h.len())
	}
}
