package replica

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/CrimsonCoder42/red_lite_cache/internal/cache"
	"github.com/CrimsonCoder42/red_lite_cache/internal/logger"
	"github.com/CrimsonCoder42/red_lite_cache/internal/sheet"
	"github.com/CrimsonCoder42/red_lite_cache/internal/ws"
	"github.com/CrimsonCoder42/red_lite_cache/protocol"
)

// Reply errors on PathReplica that are not in protocol (no reader compares them).
const (
	errMsgAlreadyRegistered = "replica already registered"
	errMsgInstanceRequired  = "instance_id required"
	errMsgRegisterFirst     = "register_replica required"
)

// replicaConn is one registered replica on the leader.
type replicaConn struct {
	c          *ws.Conn
	instanceID string
}

// session is the state of one /replica socket before and after it registers.
type replicaSession struct {
	n      *Node
	c      *ws.Conn
	remote string
	mu     sync.Mutex // guards rc: frames are read in order, but Observe reads rc from other goroutines via the hub only
	rc     *replicaConn
}

// ServeHTTP serves protocol.PathReplica on the leader: accept, read register_replica, reply
// {success, epoch}, add the connection to the hub, then one snapshot_sheet per sheet and
// snapshot_end. Every op applied after the hub add reaches the replica after its sheet's snapshot.
//
// after advo_cache/ws/handler.go:453-501 (handleRegisterReplica + sendFullState): the register
// moved from /ws to its own path, the one full_state frame became one frame per sheet taken under
// the sheet mutex (K6), the reply comes first and carries the epoch, and a replica with a higher
// epoch fences this leader (D-012).
func (n *Node) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !n.hub.enter() {
		http.Error(w, "server shutting down", http.StatusServiceUnavailable)
		return
	}
	defer n.hub.leave()
	wsc, err := websocket.Accept(w, r, nil)
	if err != nil {
		logger.L.Warn("Replica: upgrade failed", "remote", r.RemoteAddr, "err", err)
		return
	}
	wsc.SetReadLimit(n.opts.MaxFrameBytes)
	c := ws.NewConn(wsc, ws.ConnOptions{
		SendQueue: n.opts.SendQueue, PingInterval: n.opts.PingInterval,
		WriteTimeout: n.opts.WriteTimeout, Remote: r.RemoteAddr,
	})
	s := &replicaSession{n: n, c: c, remote: r.RemoteAddr}
	err = c.Run(r.Context(), s.handle)
	s.mu.Lock()
	rc := s.rc
	s.mu.Unlock()
	if rc != nil {
		n.hub.remove(rc)
		logger.L.Info("Replica: disconnected", "instance_id", rc.instanceID, "remote", r.RemoteAddr, "status", int(websocket.CloseStatus(err)))
	}
}

func (s *replicaSession) reply(resp protocol.Response) {
	b, _ := json.Marshal(resp) // a Response always encodes
	s.c.Send(b)
}

func (s *replicaSession) refuse(msg string) {
	b, _ := json.Marshal(protocol.Response{Error: msg})
	s.c.SendAndClose(b, websocket.StatusPolicyViolation, msg)
}

// handle is the read loop's callback: only register_replica is meaningful from a replica.
func (s *replicaSession) handle(frame []byte) {
	var req protocol.Request
	if err := json.Unmarshal(frame, &req); err != nil {
		s.reply(protocol.Response{Error: protocol.ErrMsgInvalidJSON})
		return
	}
	s.mu.Lock()
	registered := s.rc != nil
	s.mu.Unlock()
	switch {
	case req.Action != protocol.ActionRegisterReplica && !registered:
		s.reply(protocol.Response{ID: req.ID, Error: errMsgRegisterFirst})
	case req.Action != protocol.ActionRegisterReplica:
		s.reply(protocol.Response{ID: req.ID, Error: protocol.ErrMsgUnknownAction})
	case registered:
		logger.L.Warn("Replica: duplicate register", "instance_id", req.InstanceID, "remote", s.remote)
		s.reply(protocol.Response{ID: req.ID, Error: errMsgAlreadyRegistered})
	default:
		s.register(req)
	}
}

func (s *replicaSession) register(req protocol.Request) {
	n := s.n
	if req.InstanceID == "" {
		s.refuse(errMsgInstanceRequired)
		return
	}
	if !n.IsLeader() {
		logger.L.Warn("Replica: register refused", "instance_id", req.InstanceID, "remote", s.remote, "reason", protocol.ErrMsgNotLeader)
		s.refuse(protocol.ErrMsgNotLeader)
		return
	}
	if req.Epoch > n.Epoch() {
		logger.L.Warn("Replica: register refused", "instance_id", req.InstanceID, "remote", s.remote,
			"reason", protocol.ErrMsgStaleEpoch, "epoch", n.Epoch(), "replica_epoch", req.Epoch)
		s.refuse(protocol.ErrMsgStaleEpoch)
		n.checkPeer() // an unverified claim: only the peer's /health may fence this node
		return
	}
	epoch := n.Epoch()
	s.reply(protocol.Response{Success: true, ID: req.ID, Epoch: epoch})

	rc := &replicaConn{c: s.c, instanceID: req.InstanceID}
	s.mu.Lock()
	s.rc = rc
	s.mu.Unlock()
	old, added := n.hub.add(rc)
	if !added {
		s.c.Kick("server shutting down")
		return
	}
	if old != nil {
		old.c.Kick("replaced by a new connection of the same instance")
		logger.L.Info("Replica: replaced", "instance_id", req.InstanceID, "old_conn_id", old.c.ID(), "conn_id", s.c.ID())
	}

	// Each snapshot frame is enqueued under its sheet's mutex, so it lands in the queue exactly
	// between the ops before and after it (Observe runs under the same mutex).
	sheets := n.reg.Sheets()
	ok := true
	for _, sh := range sheets {
		sh.Snapshot(func(snap sheet.Snapshot) {
			if !ok {
				return
			}
			frame, err := json.Marshal(snapshotFrame(snap, epoch, time.Now()))
			if err != nil {
				logger.L.Error("Replica: snapshot encode failed", "sheet", snap.Name, "err", err)
				ok = false
				return
			}
			ok = s.c.Send(frame)
		})
		if !ok {
			break
		}
	}
	if ok {
		end, _ := json.Marshal(protocol.SnapshotEnd{Type: protocol.TypeSnapshotEnd, Sheets: len(sheets), Epoch: epoch})
		ok = s.c.Send(end)
	}
	if !ok {
		logger.L.Warn("Replica: send queue full", "instance_id", req.InstanceID, "phase", "snapshot", "cap", n.opts.SendQueue)
		s.c.Kick("replica send queue full")
		return
	}
	logger.L.Info("Replica: registered", "instance_id", req.InstanceID, "remote", s.remote,
		"started_at", req.StartedAt, "epoch", epoch, "replica_epoch", req.Epoch, "sheets", len(sheets))
}

// snapshotFrame is one sheet of the join snapshot.
func snapshotFrame(snap sheet.Snapshot, epoch uint64, now time.Time) protocol.ReplicaSheet {
	f := protocol.ReplicaSheet{
		Type: protocol.TypeSnapshotSheet, Sheet: snap.Name, PasswordHash: snap.PasswordHash,
		ProjectID: snap.ProjectID.String(), Data: make(map[string]json.RawMessage, len(snap.Entries)),
		Seq: snap.Seq, Epoch: epoch,
	}
	for k, e := range snap.Entries {
		f.Data[k] = e.Value
		if ms := remainingMS(e, now); ms > 0 {
			if f.ExpiresInMS == nil {
				f.ExpiresInMS = map[string]int64{}
			}
			f.ExpiresInMS[k] = ms
		}
	}
	return f
}

// remainingMS is the entry's remaining lifetime in ms; 0 = no expiry. A live entry with an expiry
// always reports at least 1, so it never reads as "no expiry".
func remainingMS(e cache.Entry, now time.Time) int64 {
	if e.ExpiresAt.IsZero() {
		return 0
	}
	return max(e.Remaining(now).Milliseconds(), 1)
}

// Observe is the replica hub (sheet.Observer): it encodes op once and enqueues it to every
// registered replica. It runs under the sheet mutex and never blocks; a full queue kicks that
// replica (it reconnects and takes a fresh snapshot).
//
// after advo_cache/ws/handler.go:503-581 (replicate* + broadcastToReplicas): a direct socket write
// per replica from the request goroutine became one non-blocking enqueue per replica, with seq and
// epoch on every frame; locks are no longer replicated (doc/ASSUMPTIONS.md "Replica: locks").
func (n *Node) Observe(op sheet.Op) {
	if !n.IsLeader() || n.hub.len() == 0 {
		return
	}
	frame, err := n.encodeOp(op)
	if err != nil {
		logger.L.Error("Replica: encode failed", "sheet", op.Sheet, "op", op.Kind.String(), "err", err)
		return
	}
	n.hub.each(func(rc *replicaConn) {
		if !rc.c.Send(frame) {
			logger.L.Warn("Replica: send queue full", "instance_id", rc.instanceID, "phase", "stream", "cap", n.opts.SendQueue)
			rc.c.Kick("replica send queue full")
		}
	})
}

func (n *Node) encodeOp(op sheet.Op) ([]byte, error) {
	epoch := n.Epoch()
	switch op.Kind {
	case sheet.OpCreate:
		return json.Marshal(protocol.ReplicaSheet{
			Type: protocol.TypeReplicateSheet, Sheet: op.Sheet, PasswordHash: op.PasswordHash,
			ProjectID: op.ProjectID.String(), Seq: op.Seq, Epoch: epoch,
		})
	case sheet.OpSet:
		return json.Marshal(protocol.Replicate{
			Type: protocol.TypeReplicate, Sheet: op.Sheet, Key: op.Key, Value: op.Value, Seq: op.Seq, Epoch: epoch,
			ExpiresInMS: remainingMS(cache.Entry{ExpiresAt: op.ExpiresAt}, time.Now()),
		})
	case sheet.OpDelete:
		return json.Marshal(protocol.ReplicaOp{Type: protocol.TypeReplicateDelete, Sheet: op.Sheet, Key: op.Key, Seq: op.Seq, Epoch: epoch})
	case sheet.OpDeletePrefix:
		return json.Marshal(protocol.ReplicaOp{Type: protocol.TypeReplicateDeletePrefix, Sheet: op.Sheet, Prefix: protocol.Ptr(op.Prefix), Seq: op.Seq, Epoch: epoch})
	case sheet.OpDeleteSuffix:
		return json.Marshal(protocol.ReplicaOp{Type: protocol.TypeReplicateDeleteSuffix, Sheet: op.Sheet, Suffix: protocol.Ptr(op.Suffix), Seq: op.Seq, Epoch: epoch})
	case sheet.OpExpire:
		return json.Marshal(protocol.ReplicaOp{Type: protocol.TypeReplicateExpire, Sheet: op.Sheet, Keys: op.Keys, Seq: op.Seq, Epoch: epoch})
	}
	return nil, errUnknownOp
}

// runLeader sends heartbeats and probes the peer until a step-down or ctx.
//
// after advo_cache/ws/handler.go:589-632 (StartHeartbeat + broadcastHeartbeat): a bare goroutine
// on a ticker that never stopped became a ctx-bound loop, the heartbeat carries epoch and
// instance id, and the same tick drives the peer probe (D-012).
func (n *Node) runLeader(ctx context.Context) {
	lctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	if n.opts.PeerAddr != "" {
		wg.Go(func() { n.probeLoop(lctx) })
	}
	defer wg.Wait() // runs after cancel (defers are LIFO)
	defer cancel()

	t := time.NewTicker(n.opts.HeartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-n.stepCh:
			if !n.IsLeader() {
				return
			}
		case <-t.C:
			n.heartbeat()
		}
	}
}

func (n *Node) heartbeat() {
	if n.hub.len() == 0 {
		return
	}
	b, _ := json.Marshal(protocol.Heartbeat{Type: protocol.TypeHeartbeat, Epoch: n.Epoch(), InstanceID: n.opts.InstanceID})
	sent := 0
	n.hub.each(func(rc *replicaConn) {
		if rc.c.Send(b) {
			sent++
			return
		}
		logger.L.Warn("Replica: send queue full", "instance_id", rc.instanceID, "phase", "heartbeat", "cap", n.opts.SendQueue)
		rc.c.Kick("replica send queue full")
	})
	logger.L.Debug("Replica: heartbeat sent", "replicas", sent, "epoch", n.Epoch())
}

// probeLoop GETs the peer's /health every heartbeat interval (1s timeout), and at once when
// checkPeer is signalled. A peer that is leader with a higher epoch fences this node: it steps down
// and follows the peer. This is the only path that steps a leader down: an epoch carried by a
// client or a registering replica is a claim anyone on the network can make.
func (n *Node) probeLoop(ctx context.Context) {
	u, err := healthURL(n.opts.PeerAddr)
	if err != nil {
		logger.L.Error("Replica: peer probe disabled", "err", err)
		return
	}
	client := &http.Client{Timeout: time.Second}
	t := time.NewTicker(n.opts.HeartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-n.probeNow: // a client or replica claimed a higher epoch: verify it now
		}
		h, err := probe(ctx, client, u)
		if err != nil {
			logger.L.Debug("Replica: peer probe failed", "peer", u, "err", err)
			continue
		}
		if h.Role == RoleLeader && h.Epoch > n.Epoch() {
			n.stepDown(h.Epoch, "peer epoch")
			return
		}
	}
}

func probe(ctx context.Context, client *http.Client, u string) (protocol.Health, error) {
	var h protocol.Health
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return h, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return h, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return h, errStatus(resp.StatusCode)
	}
	err = json.NewDecoder(resp.Body).Decode(&h)
	return h, err
}
