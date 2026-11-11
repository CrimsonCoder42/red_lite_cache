// Covers: Replica/Follow peer auth, peer /health, REDLITE_CLUSTER_TOKEN (D-031, tenancy criterion 9)
package integration

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/CrimsonCoder42/red_lite_cache/internal/config"
	"github.com/CrimsonCoder42/red_lite_cache/protocol"
	pb "github.com/CrimsonCoder42/red_lite_cache/protocol/redlitev1"
	"github.com/CrimsonCoder42/red_lite_cache/protocol/rpcwire"
)

const peerToken = "t-cluster-token"

func withClusterToken(token string) func(*config.Config) {
	return func(c *config.Config) { c.ClusterToken = token }
}

// followGRPC opens Replica/Follow with the given authorization metadata ("" = none). A stream ended
// UNAUTHENTICATED before any message maps to 401; the "body" is every message received first
// (there must be none).
func followGRPC(t *testing.T, srv *Server, auth string) (int, string) {
	t.Helper()
	cc, err := grpc.NewClient(srv.Addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	ctx := t.Context()
	if auth != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, rpcwire.AuthHeader, auth)
	}
	stream, err := pb.NewReplicaClient(cc).Follow(ctx)
	if err == nil {
		_ = stream.Send(&pb.ReplicaClientMsg{Body: &pb.ReplicaClientMsg_Register{Register: &pb.Register{InstanceId: "intruder"}}})
		var body strings.Builder
		for {
			m, rerr := stream.Recv()
			if rerr != nil {
				err = rerr
				break
			}
			body.WriteString(m.String())
		}
		if status.Code(err) == codes.Unauthenticated {
			return http.StatusUnauthorized, body.String()
		}
		t.Fatalf("Follow with authorization %q: %v (received %q)", auth, err, body.String())
	}
	if status.Code(err) == codes.Unauthenticated {
		return http.StatusUnauthorized, ""
	}
	t.Fatalf("Follow with authorization %q: %v", auth, err)
	return 0, ""
}

// TestPeerAuthReplica (criterion 9): without the token, or with a wrong one, Replica/Follow ends
// UNAUTHENTICATED (reported as 401) — no snapshot message, no registration; with it the snapshot is applied.
func TestPeerAuthReplica(t *testing.T) {
	leader := StartLeader(t, withClusterToken(peerToken))
	name := UniqueSheet(t)
	if err := NewReader(t, leader, name).Set(t.Context(), "k", 1); err != nil {
		t.Fatal(err)
	}

	for _, auth := range []string{"", "Bearer wrong", "Basic " + peerToken, peerToken} {
		code, body := followGRPC(t, leader, auth)
		if code != http.StatusUnauthorized {
			t.Fatalf("Authorization %q: status %d, want 401", auth, code)
		}
		if strings.Contains(body, protocol.TypeSnapshotSheet) || strings.Contains(body, name) {
			t.Fatalf("Authorization %q: 401 body carries snapshot bytes: %q", auth, body)
		}
	}
	// A replica configured with the wrong token never registers and gets no snapshot.
	bad := TestConfig(t)
	bad.Role, bad.LeaderAddr, bad.ClusterToken = "replica", leader.Addr, "not-the-token"
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
	a := StartLeader(t, withClusterToken(token), func(c *config.Config) { c.PeerAddr = toB.Addr })
	toA := NewProxy(t, a.Addr)
	cfg := TestConfig(t)
	cfg.Role = "replica"
	cfg.InstanceID += "-B"
	cfg.ClusterToken = token
	cfg.LeaderAddr = toA.Addr
	cfg.PeerAddr = toA.Addr
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
