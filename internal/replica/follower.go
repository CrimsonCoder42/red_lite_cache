package replica

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/CrimsonCoder42/red_lite_cache/internal/cache"
	"github.com/CrimsonCoder42/red_lite_cache/internal/logger"
	"github.com/CrimsonCoder42/red_lite_cache/internal/sheet"
	"github.com/CrimsonCoder42/red_lite_cache/protocol"
)

var (
	errUnknownOp   = errors.New("replica: unknown op kind")
	errStaleLeader = errors.New("replica: stale leader frame")
)

type errStatus int

func (e errStatus) Error() string { return fmt.Sprintf("replica: peer health status %d", int(e)) }

// Dial backoff while following: full jitter between backoffMin and a cap that doubles to backoffMax.
const (
	backoffMin = 50 * time.Millisecond
	backoffMax = 5 * time.Second
)

// follow is the replica state: dial target forever with capped jittered backoff, register, apply
// the snapshot and the stream. It returns true when the node must promote (no frame from any
// leader for HeartbeatTimeout, measured across reconnects from the last frame or boot/step-down),
// false when ctx ended.
//
// after advo_cache/replica/client.go:40-112 (Connect + listen) and :250-325 (monitorHeartbeat,
// handleLeaderDisconnect): three fixed reconnect attempts then promotion became reconnect forever
// with backoff while one monitor promotes on the heartbeat timeout; the timeout is measured from
// the last frame of any kind, not only heartbeats.
func (n *Node) follow(ctx context.Context, target string) bool {
	u, err := replicaURL(target)
	if err != nil {
		logger.L.Error("Replica: bad leader address", "err", err)
		<-ctx.Done()
		return false
	}
	fctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var promote bool
	var wg sync.WaitGroup
	wg.Go(func() {
		tick := max(n.opts.HeartbeatInterval/2, 10*time.Millisecond)
		t := time.NewTicker(tick)
		defer t.Stop()
		for {
			select {
			case <-fctx.Done():
				return
			case <-t.C:
				if n.sinceFrame() > n.opts.HeartbeatTimeout {
					logger.L.Warn("Replica: leader heartbeat timeout", "leader", u, "timeout", n.opts.HeartbeatTimeout.String(), "synced", n.synced.Load())
					promote = true
					cancel()
					return
				}
			}
		}
	})

	ceil := backoffMin
	for attempt := 1; fctx.Err() == nil; attempt++ {
		connected, err := n.session(fctx, u)
		if fctx.Err() != nil {
			break
		}
		if connected {
			ceil, attempt = backoffMin, 1
		}
		if attempt == 1 {
			logger.L.Warn("Replica: leader unreachable", "leader", u, "err", err)
		} else {
			logger.L.Debug("Replica: leader unreachable", "leader", u, "attempt", attempt, "err", err)
		}
		select {
		case <-fctx.Done():
		case <-time.After(rand.N(ceil) + time.Millisecond):
		}
		ceil = min(ceil*2, backoffMax)
	}
	cancel()
	wg.Wait()
	return promote && ctx.Err() == nil
}

// session runs one connection to the leader until it fails. connected is true once the leader
// accepted the register.
func (n *Node) session(ctx context.Context, u string) (connected bool, err error) {
	dctx, dcancel := context.WithTimeout(ctx, n.opts.HeartbeatTimeout)
	c, _, err := websocket.Dial(dctx, u, &websocket.DialOptions{HTTPHeader: bearer(n.opts.ClusterToken)})
	dcancel()
	if err != nil {
		return false, fmt.Errorf("replica: dial %s: %w", u, err)
	}
	defer c.CloseNow()
	c.SetReadLimit(n.opts.MaxFrameBytes)

	reg, _ := json.Marshal(protocol.Request{
		Action: protocol.ActionRegisterReplica, InstanceID: n.opts.InstanceID,
		StartedAt: n.startedAt.UTC().Format(time.RFC3339), Epoch: n.Epoch(),
	})
	wctx, wcancel := context.WithTimeout(ctx, n.opts.WriteTimeout)
	err = c.Write(wctx, websocket.MessageText, reg)
	wcancel()
	if err != nil {
		return false, fmt.Errorf("replica: register write: %w", err)
	}
	_, data, err := c.Read(ctx)
	if err != nil {
		return false, fmt.Errorf("replica: register reply: %w", err)
	}
	var resp protocol.Response
	if err := json.Unmarshal(data, &resp); err != nil {
		return false, fmt.Errorf("replica: register reply: %w", err)
	}
	if !resp.Success {
		return false, fmt.Errorf("replica: register refused: %s", resp.Error)
	}
	if err := n.checkEpoch(resp.Epoch, "register"); err != nil {
		return false, err
	}
	n.touch()
	logger.L.Info("Replica: connected to leader", "leader", u, "epoch", n.Epoch())

	ap := &applier{n: n, inSnapshot: true, seen: map[string]bool{}}
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			return true, fmt.Errorf("replica: read: %w", err)
		}
		if err := ap.apply(data); err != nil {
			if errors.Is(err, errStaleLeader) {
				c.Close(websocket.StatusPolicyViolation, "stale epoch")
			}
			return true, err
		}
		n.touch() // only a frame that passed the epoch check keeps this replica from promoting
	}
}

// checkEpoch drops a leader whose epoch is below ours and adopts a higher one.
func (n *Node) checkEpoch(e uint64, frame string) error {
	if e == 0 {
		return nil // a frame without an epoch (never sent by this leader; tolerated)
	}
	if cur := n.Epoch(); e < cur {
		logger.L.Warn("Replica: stale leader frame", "epoch", cur, "frame_epoch", e, "frame", frame)
		return errStaleLeader
	}
	n.adopt(e)
	return nil
}

// applier applies one connection's frames: the snapshot, then the stream.
type applier struct {
	n          *Node
	inSnapshot bool
	seen       map[string]bool // sheets named by snapshot_sheet or replicate_sheet during the snapshot
}

func (a *applier) apply(data []byte) error {
	// The first decode is protocol.ReplicaOp: it carries type, sheet and epoch (every leader frame
	// has them) and is the whole frame for the delete-shaped ops, so those decode once.
	var h protocol.ReplicaOp
	if err := json.Unmarshal(data, &h); err != nil {
		return fmt.Errorf("replica: decode frame: %w", err)
	}
	if err := a.n.checkEpoch(h.Epoch, h.Type); err != nil {
		return err
	}
	now := time.Now()
	switch h.Type {
	case protocol.TypeHeartbeat:
		return nil
	case protocol.TypeProjectSuspended, protocol.TypeProjectResumed, protocol.TypeKeyRevoked, protocol.TypeSessionKill:
		return a.control(data, h.Type)
	case protocol.TypeSnapshotSheet, protocol.TypeReplicateSheet:
		var f protocol.ReplicaSheet
		if err := json.Unmarshal(data, &f); err != nil {
			return fmt.Errorf("replica: decode %s: %w", h.Type, err)
		}
		return a.sheet(f, now)
	case protocol.TypeSnapshotEnd:
		var f protocol.SnapshotEnd
		if err := json.Unmarshal(data, &f); err != nil {
			return fmt.Errorf("replica: decode %s: %w", h.Type, err)
		}
		a.end(f)
		return nil
	case protocol.TypeReplicate:
		var f protocol.Replicate
		if err := json.Unmarshal(data, &f); err != nil {
			return fmt.Errorf("replica: decode %s: %w", h.Type, err)
		}
		op := sheet.Op{Kind: sheet.OpSet, Key: f.Key, Value: f.Value, Seq: f.Seq}
		if f.ExpiresInMS > 0 {
			op.ExpiresAt = now.Add(time.Duration(f.ExpiresInMS) * time.Millisecond)
		}
		a.stream(h, op)
		return nil
	case protocol.TypeReplicateDelete, protocol.TypeReplicateDeletePrefix, protocol.TypeReplicateDeleteSuffix, protocol.TypeReplicateExpire:
		f := h
		op := sheet.Op{Seq: f.Seq, Key: f.Key, Keys: f.Keys}
		switch h.Type {
		case protocol.TypeReplicateDelete:
			op.Kind = sheet.OpDelete
		case protocol.TypeReplicateDeletePrefix:
			op.Kind = sheet.OpDeletePrefix
			if f.Prefix != nil {
				op.Prefix = *f.Prefix
			}
		case protocol.TypeReplicateDeleteSuffix:
			op.Kind = sheet.OpDeleteSuffix
			if f.Suffix == nil {
				return nil // never sent: an absent suffix is nothing to delete
			}
			op.Suffix = *f.Suffix
		case protocol.TypeReplicateExpire:
			op.Kind = sheet.OpExpire
		}
		a.stream(h, op)
		return nil
	}
	logger.L.Debug("Replica: unknown frame type", "type", h.Type)
	return nil
}

// control hands one tenancy control frame (already past the epoch check) to Options.OnControl;
// the replica applies nothing itself — internal/app owns the tenant directory and sessions.
func (a *applier) control(data []byte, typ string) error {
	var f protocol.ReplicaControl
	if err := json.Unmarshal(data, &f); err != nil {
		return fmt.Errorf("replica: decode %s: %w", typ, err)
	}
	logger.L.Info("Replica: control received", "type", f.Type, "project", f.Project, "key_id", f.KeyID, "session", f.Session, "epoch", f.Epoch)
	if a.n.opts.OnControl != nil {
		a.n.opts.OnControl(f)
	}
	return nil
}

// bearer is the Authorization header carrying the cluster token (D-031); nil when there is none.
func bearer(token string) http.Header {
	if token == "" {
		return nil
	}
	return http.Header{"Authorization": {"Bearer " + token}}
}

// sheet applies snapshot_sheet (identity + whole state) or replicate_sheet (identity only).
func (a *applier) sheet(f protocol.ReplicaSheet, now time.Time) error {
	project := sheet.DefaultProject
	if f.ProjectID != "" {
		p, err := uuid.Parse(f.ProjectID)
		if err != nil {
			return fmt.Errorf("replica: %s %q: project id: %w", f.Type, f.Sheet, err)
		}
		project = p
	}
	s := a.n.reg.Restore(project, f.Sheet, f.PasswordHash)
	if a.inSnapshot {
		a.seen[f.Sheet] = true
	}
	if f.Type != protocol.TypeSnapshotSheet {
		return nil
	}
	entries := make(map[string]cache.Entry, len(f.Data))
	for k, v := range f.Data {
		e := cache.Entry{Value: v}
		if ms := f.ExpiresInMS[k]; ms > 0 {
			e.ExpiresAt = now.Add(time.Duration(ms) * time.Millisecond)
		}
		entries[k] = e
	}
	s.Replace(sheet.Snapshot{Name: f.Sheet, ProjectID: project, PasswordHash: f.PasswordHash, Seq: f.Seq, Entries: entries})
	return nil
}

// end closes the snapshot: every local sheet the leader did not name is removed.
func (a *applier) end(f protocol.SnapshotEnd) {
	removed := 0
	for _, s := range a.n.reg.Sheets() {
		if !a.seen[s.Name()] {
			a.n.reg.Remove(s.Name())
			removed++
		}
	}
	a.inSnapshot = false
	a.seen = nil
	a.n.synced.Store(true)
	logger.L.Info("Replica: snapshot applied", "sheets", f.Sheets, "removed", removed, "epoch", a.n.Epoch())
}

// stream applies one live op to its sheet; an unknown sheet is ignored (its snapshot or
// replicate_sheet carries its state).
func (a *applier) stream(h protocol.ReplicaOp, op sheet.Op) {
	s, ok := a.n.reg.Get(h.Sheet)
	if !ok {
		logger.L.Debug("Replica: unknown sheet", "sheet", h.Sheet, "type", h.Type)
		return
	}
	s.ApplyReplicated(op)
}
