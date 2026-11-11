package replica

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"

	"github.com/CrimsonCoder42/red_lite_cache/internal/rpc"
	"github.com/CrimsonCoder42/red_lite_cache/protocol"
	pb "github.com/CrimsonCoder42/red_lite_cache/protocol/redlitev1"
	"github.com/CrimsonCoder42/red_lite_cache/protocol/rpcwire"
)

// The leader → replica stream over gRPC (D-048): the Replica service (Follow, Probe), the
// follower's link and the peer prober. The cluster token rides as call credentials
// (`authorization: Bearer <token>`); the server checks it in rpc.StreamAuth / Probe (D-031).

// grpcTarget is addr as a gRPC dial target: "host:port" (a "grpc://" prefix is accepted).
func grpcTarget(addr string) string { return strings.TrimPrefix(addr, "grpc://") }

func withToken(ctx context.Context, token string) context.Context {
	if token == "" {
		return ctx
	}
	return metadata.AppendToOutgoingContext(ctx, rpcwire.AuthHeader, "Bearer "+token)
}

// dialGRPC is a client connection to the peer's client port: plaintext (the cluster runs on a
// private network or behind TLS termination), keepalive so a dead peer is noticed
// within HeartbeatTimeout, and the replica stream's receive limit.
func (n *Node) dialGRPC(addr string) (*grpc.ClientConn, error) {
	return grpc.NewClient(grpcTarget(addr),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{Time: max(n.opts.HeartbeatInterval, 5*time.Second), Timeout: n.opts.HeartbeatTimeout, PermitWithoutStream: true}),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(int(max(n.opts.MaxFrameBytes, rpcwire.DefaultChunkBytes*2)))),
	)
}

// ── leader side ─────────────────────────────────────────────────────────────────────────────

type replicaService struct {
	pb.UnimplementedReplicaServer
	n *Node
}

// ReplicaService is the gRPC Replica service of this node, registered on the client port's server.
func (n *Node) ReplicaService() pb.ReplicaServer { return &replicaService{n: n} }

// Follow serves one replica stream (serveReplica),
// its frames sent as ReplicaServerMsgs (a snapshot sheet as sorted chunks).
func (s *replicaService) Follow(stream grpc.BidiStreamingServer[pb.ReplicaClientMsg, pb.ReplicaServerMsg]) error {
	n := s.n
	if !n.hub.enter() {
		return rpcwire.CloseStatus(rpcwire.CloseGoingAway, "server shutting down")
	}
	defer n.hub.leave()
	ctx := stream.Context()
	t := rpc.NewStreamTransport(ctx,
		func() ([]byte, error) {
			m, err := stream.Recv()
			if err != nil {
				return nil, err
			}
			req, err := rpcwire.RegisterRequest(m)
			if err != nil {
				return nil, err
			}
			return json.Marshal(req)
		},
		func(frame []byte) error {
			msgs, err := rpcwire.ReplicaMsgs(frame, rpcwire.DefaultChunkBytes)
			if err != nil {
				return err
			}
			for _, m := range msgs {
				if err := stream.Send(m); err != nil {
					return err
				}
			}
			return nil
		})
	remote := ""
	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		remote = p.Addr.String()
	}
	n.serveReplica(ctx, t, remote)
	return t.Status()
}

// Probe answers a peer's fencing check. The token is checked here: rpc.UnaryAuth lets this method
// through to its own check.
func (s *replicaService) Probe(ctx context.Context, _ *pb.ProbeRequest) (*pb.ProbeReply, error) {
	n := s.n
	md, _ := metadata.FromIncomingContext(ctx)
	if !rpc.TokenOK(md, n.opts.ClusterToken) {
		return nil, rpcwire.CloseStatus(rpcwire.ClosePolicyViolation, "cluster token required")
	}
	return &pb.ProbeReply{Role: n.Role(), Epoch: n.Epoch(), InstanceId: n.opts.InstanceID}, nil
}

// ── follower side ───────────────────────────────────────────────────────────────────────────

// grpcLink is one follower connection to the leader's Replica service.
type grpcLink struct {
	conn   *grpc.ClientConn
	cancel context.CancelFunc
	stream grpc.BidiStreamingClient[pb.ReplicaClientMsg, pb.ReplicaServerMsg]
	asm    rpcwire.SnapshotAssembler
}

func (n *Node) dialLinkGRPC(ctx context.Context, addr string) (link, error) {
	conn, err := n.dialGRPC(addr)
	if err != nil {
		return nil, fmt.Errorf("replica: dial %s: %w", addr, err)
	}
	sctx, cancel := context.WithCancel(withToken(ctx, n.opts.ClusterToken))
	stream, err := pb.NewReplicaClient(conn).Follow(sctx)
	if err != nil {
		cancel()
		conn.Close()
		return nil, fmt.Errorf("replica: follow %s: %w", addr, err)
	}
	return &grpcLink{conn: conn, cancel: cancel, stream: stream}, nil
}

func (l *grpcLink) register(_ context.Context, req protocol.Request) error {
	return l.stream.Send(rpcwire.RegisterMsg(req))
}

// read returns the next whole frame: snapshot chunks are gathered until their sheet's last.
func (l *grpcLink) read(context.Context) ([]byte, error) {
	for {
		m, err := l.stream.Recv()
		if err != nil {
			return nil, err
		}
		frame, err := rpcwire.ReplicaFrameFor(m, &l.asm)
		if err != nil {
			return nil, err
		}
		if frame != nil {
			return frame, nil
		}
	}
}

func (l *grpcLink) refuse(string) { l.cancel() }

func (l *grpcLink) close() {
	l.cancel()
	l.conn.Close()
}

// ── peer probe ──────────────────────────────────────────────────────────────────────────────

// grpcProber probes a peer's Replica.Probe; close releases its connection.
func (n *Node) grpcProber(addr string) (func(context.Context) (protocol.Health, error), func(), error) {
	conn, err := n.dialGRPC(addr)
	if err != nil {
		return nil, nil, err
	}
	client := pb.NewReplicaClient(conn)
	probeFn := func(ctx context.Context) (protocol.Health, error) {
		pctx, cancel := context.WithTimeout(withToken(ctx, n.opts.ClusterToken), time.Second)
		defer cancel()
		r, err := client.Probe(pctx, &pb.ProbeRequest{})
		if err != nil {
			return protocol.Health{}, err
		}
		if r == nil {
			return protocol.Health{}, errors.New("replica: empty probe reply")
		}
		return protocol.Health{Status: "ok", Role: r.GetRole(), Epoch: r.GetEpoch(), InstanceID: r.GetInstanceId()}, nil
	}
	return probeFn, func() { conn.Close() }, nil
}
