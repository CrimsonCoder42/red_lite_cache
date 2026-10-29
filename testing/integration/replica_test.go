// Covers: internal/replica (register_replica, snapshot_sheet, snapshot_end, replicate*, heartbeat, promotion, fencing)
package integration

import (
	"fmt"
	"maps"
	"testing"
	"time"

	"github.com/CrimsonCoder42/red_lite_cache/internal/config"
	"github.com/CrimsonCoder42/red_lite_cache/internal/sheet"
	"github.com/CrimsonCoder42/red_lite_cache/protocol"
)

// sheetState is a sheet's entries (key → raw JSON) and seq on srv; ok is false without the sheet.
func sheetState(srv *Server, name string) (data map[string]string, seq uint64, ok bool) {
	s, found := srv.App.Registry().Get(name)
	if !found {
		return nil, 0, false
	}
	s.Snapshot(func(snap sheet.Snapshot) {
		data = make(map[string]string, len(snap.Entries))
		for k, e := range snap.Entries {
			data[k] = string(e.Value)
		}
		seq = snap.Seq
	})
	return data, seq, true
}

// sameState reports whether a and b hold the same entries and seq for name.
func sameState(a, b *Server, name string) bool {
	da, sa, oka := sheetState(a, name)
	db, sb, okb := sheetState(b, name)
	return oka && okb && sa == sb && maps.Equal(da, db)
}

// TestReplicaLoop (REPLICA loop, K12): a reader writes through the leader, the replica follows,
// the leader dies, the replica promotes with epoch 2, the reader fails over and keeps working,
// and the leader's password is still required.
func TestReplicaLoop(t *testing.T) {
	leader := StartLeader(t)
	rep := StartReplica(t, leader)
	name := UniqueSheet(t)
	r := NewReaderAt(t, leader.WSURL, rep.WSURL, name)
	ctx := t.Context()

	for k, v := range map[string]int{"a": 1, "b": 2, "user:1": 3, "user:2": 4, "x:tok": 5} {
		if err := r.Set(ctx, k, v); err != nil {
			t.Fatalf("set %s: %v", k, err)
		}
	}
	if err := r.Delete(ctx, "b"); err != nil {
		t.Fatal(err)
	}
	if err := r.DeletePrefix(ctx, "user:"); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"a": "1", "x:tok": "5"}
	Eventually(t, "replica holds the leader's state", func() bool {
		d, _, ok := sheetState(rep, name)
		return ok && maps.Equal(d, want) && sameState(leader, rep, name)
	})
	if rep.App.Node().IsLeader() || rep.App.Node().Epoch() != 1 {
		t.Fatalf("replica before failover: leader=%v epoch=%d", rep.App.Node().IsLeader(), rep.App.Node().Epoch())
	}

	leader.Stop()
	stopped := time.Now()
	Eventually(t, "replica promoted", rep.App.Node().IsLeader)
	if took, limit := time.Since(stopped), rep.Cfg.HeartbeatTimeout+time.Second; took > limit {
		t.Fatalf("promotion took %s, want within %s", took, limit)
	}
	if e := rep.App.Node().Epoch(); e != 2 {
		t.Fatalf("promoted epoch = %d, want 2", e)
	}
	if h := Health(t, rep); h.Role != "leader" || h.Epoch != 2 {
		t.Fatalf("health after promotion = %+v", h)
	}

	Eventually(t, "reader writes through the promoted replica", func() bool { return r.Set(ctx, "after", 6) == nil })
	if !r.IsConnected() {
		t.Fatal("reader not connected after a successful write")
	}
	var a int
	if !r.Get("a", &a) || a != 1 {
		t.Fatalf("reader Get(a) after failover = %d", a)
	}
	if d, _, _ := sheetState(rep, name); d["after"] != "6" {
		t.Fatalf("write after failover not on the promoted replica: %v", d)
	}

	// K12: the bcrypt hash travelled with the sheet; a wrong password is refused.
	c := Dial(t, rep.WSURL)
	if resp, _ := c.Open(name, "wrong"); resp.Success || resp.Error != protocol.ErrMsgInvalidPassword {
		t.Fatalf("wrong password on promoted replica = %+v", resp)
	}
	if resp, _ := Dial(t, rep.WSURL).Open(name, "pw"); !resp.Success || resp.Epoch != 2 {
		t.Fatalf("right password on promoted replica = %+v", resp)
	}
}

// TestReplicaRefusesOpenBeforePromotion: while its leader is alive a replica answers open with
// "not leader" and reports role replica at the leader's epoch.
func TestReplicaRefusesOpenBeforePromotion(t *testing.T) {
	leader := StartLeader(t)
	rep := StartReplica(t, leader)
	c := Dial(t, rep.WSURL)
	if r := c.Do(protocol.Request{Action: protocol.ActionOpen, Sheet: UniqueSheet(t), Password: "pw"}); r.Error != protocol.ErrMsgNotLeader {
		t.Fatalf("open on a following replica = %+v", r)
	}
	if h := Health(t, rep); h.Role != "replica" || h.Epoch != 1 {
		t.Fatalf("replica health = %+v", h)
	}
	// Heartbeats keep it a replica well past the timeout.
	time.Sleep(3 * rep.Cfg.HeartbeatTimeout)
	if rep.App.Node().IsLeader() {
		t.Fatal("replica promoted while its leader was alive")
	}
}

// TestSnapshotThenStream (K6): keys set before the replica joins and while its snapshot is being
// taken all arrive, and the replica's seq equals the leader's.
func TestSnapshotThenStream(t *testing.T) {
	leader := StartLeader(t)
	name := UniqueSheet(t)
	r := NewReader(t, leader, name)
	ctx := t.Context()
	for i := range 200 {
		if err := r.Set(ctx, fmt.Sprintf("pre-%d", i), i); err != nil {
			t.Fatal(err)
		}
	}
	// A second sheet that only the snapshot carries.
	other := UniqueSheet(t)
	if err := NewReader(t, leader, other).Set(ctx, "k", 1); err != nil {
		t.Fatal(err)
	}

	errs := make(chan error, 1)
	go func() {
		for i := range 300 {
			if err := r.Set(ctx, fmt.Sprintf("live-%d", i), i); err != nil {
				errs <- err
				return
			}
			if i%50 == 0 {
				if err := r.DeletePrefix(ctx, fmt.Sprintf("pre-%d", i/50)); err != nil {
					errs <- err
					return
				}
			}
		}
		errs <- nil
	}()
	rep := StartReplica(t, leader)
	if err := <-errs; err != nil {
		t.Fatalf("writes during the snapshot: %v", err)
	}
	Eventually(t, "replica equals leader (entries and seq)", func() bool { return sameState(leader, rep, name) })
	Eventually(t, "second sheet replicated", func() bool { return sameState(leader, rep, other) })
	_, ls, _ := sheetState(leader, name)
	_, rs, _ := sheetState(rep, name)
	if ls != rs || ls < 500 {
		t.Fatalf("seq leader=%d replica=%d", ls, rs)
	}
}

// TestDuplicateRegisterReplica: a second register on the same connection is refused; a new
// connection with the same instance id replaces (closes) the first.
func TestDuplicateRegisterReplica(t *testing.T) {
	leader := StartLeader(t)
	if err := NewReader(t, leader, UniqueSheet(t)).Set(t.Context(), "k", 1); err != nil {
		t.Fatal(err)
	}
	reg := protocol.Request{Action: protocol.ActionRegisterReplica, InstanceID: "dup", StartedAt: time.Now().UTC().Format(time.RFC3339)}

	first := Dial(t, leader.ReplicaURL)
	if r := first.Do(reg); !r.Success || r.Epoch != 1 {
		t.Fatalf("register = %+v", r)
	}
	var snap protocol.ReplicaSheet
	first.NextType(protocol.TypeSnapshotSheet, &snap)
	if len(snap.PasswordHash) == 0 || snap.Epoch != 1 {
		t.Fatalf("snapshot_sheet without hash or epoch: %+v", snap)
	}
	var end protocol.SnapshotEnd
	first.NextType(protocol.TypeSnapshotEnd, &end)
	if end.Sheets != 1 || end.Epoch != 1 {
		t.Fatalf("snapshot_end = %+v", end)
	}
	if r := first.Do(reg); r.Success || r.Error == "" {
		t.Fatalf("duplicate register on the same conn = %+v", r)
	}
	if n := leader.App.Node().Replicas(); n != 1 {
		t.Fatalf("replicas = %d, want 1", n)
	}

	second := Dial(t, leader.ReplicaURL)
	if r := second.Do(reg); !r.Success {
		t.Fatalf("register from a new conn = %+v", r)
	}
	first.ExpectClosed(Timeout)
	Eventually(t, "one replica registered", func() bool { return leader.App.Node().Replicas() == 1 })
	var hb protocol.Heartbeat
	second.NextType(protocol.TypeHeartbeat, &hb)
	if hb.Epoch != 1 || hb.InstanceID != leader.Cfg.InstanceID {
		t.Fatalf("heartbeat = %+v", hb)
	}

	// A register carrying a higher epoch is refused, but an unverified claim never fences the
	// leader (only the peer probe does; this leader has no peer): it keeps leading at epoch 1.
	third := Dial(t, leader.ReplicaURL)
	higher := reg
	higher.InstanceID, higher.Epoch = "newer", 7
	if r := third.Do(higher); r.Error != protocol.ErrMsgStaleEpoch {
		t.Fatalf("register with epoch 7 = %+v", r)
	}
	time.Sleep(3 * leader.Cfg.HeartbeatInterval)
	if n := leader.App.Node(); !n.IsLeader() || n.Epoch() != 1 {
		t.Fatalf("leader after an unverified epoch-7 register: leader=%v epoch=%d", n.IsLeader(), n.Epoch())
	}
}

// pair is a leader A and a replica B with each one's view of the other behind a cuttable proxy:
// B → A (LeaderAddr and PeerAddr) through toA, A → B (PeerAddr) through toB.
type pair struct {
	A, B      *Server
	toA, toB  *Proxy
	sheetName string
}

func startPair(t *testing.T) *pair {
	t.Helper()
	lnB := Listen(t)
	toB := NewProxy(t, lnB.Addr().String())
	a := StartLeader(t, func(c *config.Config) { c.PeerAddr = "ws://" + toB.Addr })
	toA := NewProxy(t, a.Addr)
	cfg := TestConfig(t)
	cfg.Role = "replica"
	cfg.InstanceID += "-B"
	cfg.LeaderAddr = "ws://" + toA.Addr + protocol.PathReplica
	cfg.PeerAddr = "ws://" + toA.Addr
	b := StartOn(t, lnB, cfg)
	Eventually(t, "B registered with A", func() bool { return a.App.Node().Replicas() == 1 })
	p := &pair{A: a, B: b, toA: toA, toB: toB, sheetName: UniqueSheet(t)}
	if err := NewReader(t, a, p.sheetName).Set(t.Context(), "before", 1); err != nil {
		t.Fatal(err)
	}
	Eventually(t, "B has A's write", func() bool { return sameState(a, b, p.sheetName) })
	return p
}

// partition cuts both directions and waits for B to promote with epoch 2.
func (p *pair) partition(t *testing.T) {
	t.Helper()
	p.toA.Block()
	p.toB.Block()
	Eventually(t, "B promoted", p.B.App.Node().IsLeader)
	if e := p.B.App.Node().Epoch(); e != 2 {
		t.Fatalf("B epoch = %d, want 2", e)
	}
}

func (p *pair) heal() {
	p.toA.Unblock()
	p.toB.Unblock()
}

// converged asserts no split brain: exactly one node reports leader on /health (B, epoch 2), A
// follows it at epoch 2, and a write through B reaches A.
func (p *pair) converged(t *testing.T) {
	t.Helper()
	Eventually(t, "A stepped down and follows B", func() bool {
		return !p.A.App.Node().IsLeader() && p.A.App.Node().Epoch() == 2 && sameState(p.B, p.A, p.sheetName)
	})
	leaders := 0
	for _, s := range []*Server{p.A, p.B} {
		if h := Health(t, s); h.Role == "leader" {
			leaders++
			if s != p.B || h.Epoch != 2 {
				t.Fatalf("unexpected leader %+v", h)
			}
		}
	}
	if leaders != 1 {
		t.Fatalf("%d leaders after convergence, want 1", leaders)
	}
	if err := NewReader(t, p.B, p.sheetName).Set(t.Context(), "after", 2); err != nil {
		t.Fatal(err)
	}
	Eventually(t, "B's write reaches A", func() bool {
		d, _, _ := sheetState(p.A, p.sheetName)
		return d["after"] == "2"
	})
}

// TestFencingStepDown (K11, D-012): a partition promotes B; A cannot see it and stays leader at
// epoch 1 (the accepted split-brain window); when the partition heals A's peer probe sees B as
// leader at epoch 2, A steps down, follows B, and the pair converges on one leader.
func TestFencingStepDown(t *testing.T) {
	p := startPair(t)
	p.partition(t)
	if !p.A.App.Node().IsLeader() || p.A.App.Node().Epoch() != 1 {
		t.Fatalf("A during the partition: leader=%v epoch=%d", p.A.App.Node().IsLeader(), p.A.App.Node().Epoch())
	}
	p.heal()
	p.converged(t)
}

// TestFencingByReaderEpoch (K11, D-012): a reader that saw epoch 2 on B fails over to A (epoch 1):
// A refuses it (stale epoch) and steps down at once, before any peer probe could tell it.
func TestFencingByReaderEpoch(t *testing.T) {
	p := startPair(t)
	p.partition(t)
	toB := NewProxy(t, p.B.Addr) // the reader's own path to B
	r := NewReaderAt(t, "ws://"+toB.Addr+protocol.PathWS, p.A.WSURL, p.sheetName)
	Eventually(t, "reader on B", r.IsConnected)

	toB.Block() // the reader loses B and tries A, which answers with the stale epoch 1
	// A cannot reach B to verify the reader's claim, so it stays leader — but the reader, having
	// seen epoch 2, refuses it: no client writes reach the stale leader (the read-side fence).
	Eventually(t, "reader notices B is gone", func() bool { return !r.IsConnected() })
	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if r.IsConnected() {
			t.Fatal("reader connected to the stale A (epoch 1) after seeing epoch 2")
		}
		time.Sleep(20 * time.Millisecond)
	}

	toB.Unblock()
	p.heal() // A's peer probe now sees B leader at epoch 2 and steps down
	Eventually(t, "A stepped down after verifying the peer", func() bool {
		return !p.A.App.Node().IsLeader() && p.A.App.Node().Epoch() == 2
	})
	p.converged(t)
	Eventually(t, "reader back on B", r.IsConnected)
}
