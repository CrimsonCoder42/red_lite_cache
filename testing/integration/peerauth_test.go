// Covers: /replica peer auth, peer /health, REDLITE_CLUSTER_TOKEN (D-031, tenancy criterion 9)
package integration

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/CrimsonCoder42/red_lite_cache/internal/config"
	"github.com/CrimsonCoder42/red_lite_cache/protocol"
)

const peerToken = "t-cluster-token"

func withClusterToken(token string) func(*config.Config) {
	return func(c *config.Config) { c.ClusterToken = token }
}

// dialReplicaPath tries the /replica upgrade with the given Authorization header ("" = none) and
// returns the HTTP status and the first bytes of the body. A successful upgrade fails the test.
func dialReplicaPath(t *testing.T, srv *Server, auth string) (int, string) {
	t.Helper()
	opts := &websocket.DialOptions{}
	if auth != "" {
		opts.HTTPHeader = http.Header{"Authorization": {auth}}
	}
	c, resp, err := websocket.Dial(t.Context(), srv.ReplicaURL, opts)
	if err == nil {
		c.CloseNow()
		t.Fatalf("/replica upgraded with Authorization %q", auth)
	}
	if resp == nil {
		t.Fatalf("/replica dial: no HTTP response: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// TestPeerAuthReplica (criterion 9): without the token, or with a wrong one, /replica answers 401
// before the upgrade — no snapshot frame, no registration; with it the snapshot is applied.
func TestPeerAuthReplica(t *testing.T) {
	leader := StartLeader(t, withClusterToken(peerToken))
	name := UniqueSheet(t)
	if err := NewReader(t, leader, name).Set(t.Context(), "k", 1); err != nil {
		t.Fatal(err)
	}

	for _, auth := range []string{"", "Bearer wrong", "Basic " + peerToken, peerToken} {
		code, body := dialReplicaPath(t, leader, auth)
		if code != http.StatusUnauthorized {
			t.Fatalf("Authorization %q: status %d, want 401", auth, code)
		}
		if strings.Contains(body, protocol.TypeSnapshotSheet) || strings.Contains(body, name) {
			t.Fatalf("Authorization %q: 401 body carries snapshot bytes: %q", auth, body)
		}
	}
	// A plain GET (no upgrade headers) is refused the same way.
	resp, err := http.Get(leader.URL + protocol.PathReplica)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("plain GET /replica: status %d, want 401", resp.StatusCode)
	}

	// A replica configured with the wrong token never registers and gets no snapshot.
	bad := TestConfig(t)
	bad.Role, bad.LeaderAddr, bad.ClusterToken = "replica", leader.ReplicaURL, "not-the-token"
	bad.InstanceID += "-bad"
	badSrv := Start(t, bad)
	time.Sleep(3 * leader.Cfg.HeartbeatInterval)
	if n := leader.App.Node().Replicas(); n != 0 {
		t.Fatalf("%d replicas registered with a wrong token", n)
	}
	if _, ok := badSrv.App.Registry().Get(name); ok {
		t.Fatal("wrong-token replica holds the leader's sheet")
	}
	badSrv.Stop()

	// The right token (StartReplica passes the leader's): snapshot applied.
	rep := StartReplica(t, leader)
	Eventually(t, "replica has the leader's sheet", func() bool { return sameState(leader, rep, name) })
}

// TestPeerAuthHealth: /health is 200 {"status":"ok"} to anyone; role, epoch and instance id only
// with the cluster token.
func TestPeerAuthHealth(t *testing.T) {
	leader := StartLeader(t, withClusterToken(peerToken))
	for _, tok := range []string{"", "wrong"} {
		h, err := TryHealthToken(leader.URL, tok)
		if err != nil {
			t.Fatal(err)
		}
		if h != (protocol.Health{Status: "ok"}) {
			t.Fatalf("health with token %q = %+v; want status only", tok, h)
		}
	}
	h, err := TryHealthToken(leader.URL, peerToken)
	if err != nil {
		t.Fatal(err)
	}
	if h.Status != "ok" || h.Role != "leader" || h.Epoch != 1 || h.InstanceID == "" {
		t.Fatalf("health with the token = %+v", h)
	}
}

// startPairToken is startPair (replica_test.go) with the cluster token set on both nodes: the
// follower dial and both peer probes must carry it.
func startPairToken(t *testing.T, token string) *pair {
	t.Helper()
	lnB := Listen(t)
	toB := NewProxy(t, lnB.Addr().String())
	a := StartLeader(t, withClusterToken(token), func(c *config.Config) { c.PeerAddr = "ws://" + toB.Addr })
	toA := NewProxy(t, a.Addr)
	cfg := TestConfig(t)
	cfg.Role = "replica"
	cfg.InstanceID += "-B"
	cfg.ClusterToken = token
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

// TestPeerAuthFencing (K11, D-012 with D-031): TestFencingStepDown with tokens on both nodes — the
// peer probe reads role and epoch only because it sends the token, so A still steps down.
func TestPeerAuthFencing(t *testing.T) {
	p := startPairToken(t, peerToken)
	p.partition(t)
	p.heal()
	p.converged(t)
}
