// Covers: internal/replica (follower control frames — D-027/D-028/D-030; cluster token header — D-031)
package replica

import (
	"encoding/json"
	"errors"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/CrimsonCoder42/red_lite_cache/internal/rpc"
	"github.com/CrimsonCoder42/red_lite_cache/internal/sheet"
	"github.com/CrimsonCoder42/red_lite_cache/protocol"
	pb "github.com/CrimsonCoder42/red_lite_cache/protocol/redlitev1"
	"github.com/CrimsonCoder42/red_lite_cache/protocol/rpcwire"
)

func testReplicaNode(t *testing.T, onControl func(protocol.ReplicaControl)) *Node {
	t.Helper()
	n, err := New(t.Context(), sheet.NewRegistry(sheet.Options{}), Options{
		Role: RoleReplica, HeartbeatInterval: 100 * time.Millisecond, HeartbeatTimeout: time.Second,
		OnControl: onControl,
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// Each of the four control types reaches OnControl decoded; a frame below the replica's epoch is
// dropped as a stale leader frame and never reaches it; nil OnControl ignores the frame.
func TestFollowerControlFrames(t *testing.T) {
	var got []protocol.ReplicaControl
	n := testReplicaNode(t, func(c protocol.ReplicaControl) { got = append(got, c) })
	n.adopt(3)
	ap := &applier{n: n}
	sent := []protocol.ReplicaControl{
		{Type: protocol.TypeProjectSuspended, Project: "p1", Epoch: 3},
		{Type: protocol.TypeProjectResumed, Project: "p1", Epoch: 3},
		{Type: protocol.TypeKeyRevoked, Project: "p1", KeyID: "k1", Epoch: 3},
		{Type: protocol.TypeSessionKill, Session: "s1", Epoch: 4},
	}
	for _, c := range sent {
		b, _ := json.Marshal(c)
		if err := ap.apply(b); err != nil {
			t.Fatalf("apply %s: %v", c.Type, err)
		}
	}
	if len(got) != len(sent) {
		t.Fatalf("OnControl got %d frames, want %d: %+v", len(got), len(sent), got)
	}
	for i := range sent {
		if got[i] != sent[i] {
			t.Fatalf("frame %d: got %+v, want %+v", i, got[i], sent[i])
		}
	}
	if n.Epoch() != 4 {
		t.Fatalf("epoch = %d, want 4 (adopted from the control frame)", n.Epoch())
	}
	stale, _ := json.Marshal(protocol.ReplicaControl{Type: protocol.TypeSessionKill, Session: "s2", Epoch: 2})
	if err := ap.apply(stale); !errors.Is(err, errStaleLeader) {
		t.Fatalf("stale control frame: err = %v, want errStaleLeader", err)
	}
	if len(got) != len(sent) {
		t.Fatal("a stale control frame reached OnControl")
	}

	quiet := &applier{n: testReplicaNode(t, nil)}
	b, _ := json.Marshal(sent[0])
	if err := quiet.apply(b); err != nil {
		t.Fatalf("apply without OnControl: %v", err)
	}
}

// D-031: Replica/Follow and Replica/Probe refuse a missing or wrong cluster token (UNAUTHENTICATED,
// no hub entry); the peer prober sends it as call credentials.
func TestClusterTokenRefusalAndProbeHeader(t *testing.T) {
	n, err := New(t.Context(), sheet.NewRegistry(sheet.Options{}), Options{
		Role: RoleLeader, HeartbeatInterval: 100 * time.Millisecond, HeartbeatTimeout: time.Second,
		ClusterToken: "tok", InstanceID: "leader-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	srv, _ := rpc.NewServer(rpc.ServerOptions{ClusterToken: "tok", MaxRecvBytes: 1 << 20, KeepaliveTime: time.Minute, KeepaliveWait: time.Second})
	pb.RegisterReplicaServer(srv, n.ReplicaService())
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	t.Cleanup(srv.Stop)
	cc, err := grpc.NewClient(ln.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cc.Close() })
	client := pb.NewReplicaClient(cc)

	for _, auth := range []string{"", "Bearer nope", "tok"} {
		ctx := t.Context()
		if auth != "" {
			ctx = metadata.AppendToOutgoingContext(ctx, rpcwire.AuthHeader, auth)
		}
		f, err := client.Follow(ctx)
		if err == nil {
			_, err = f.Recv()
		}
		if status.Code(err) != codes.Unauthenticated {
			t.Fatalf("Follow with %q: %v, want UNAUTHENTICATED", auth, err)
		}
		if _, err := client.Probe(ctx, &pb.ProbeRequest{}); status.Code(err) != codes.Unauthenticated {
			t.Fatalf("Probe with %q: %v, want UNAUTHENTICATED", auth, err)
		}
	}
	if n.hub.len() != 0 {
		t.Fatalf("%d replicas registered without the token", n.hub.len())
	}

	probe, closeFn, err := n.grpcProber(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer closeFn()
	h, err := probe(t.Context())
	if err != nil || h.Role != RoleLeader || h.Epoch != 1 || h.InstanceID != "leader-1" {
		t.Fatalf("probe with the token = %+v, %v", h, err)
	}
}
