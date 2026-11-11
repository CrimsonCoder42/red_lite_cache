package rpc

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	pb "github.com/CrimsonCoder42/red_lite_cache/protocol/redlitev1"
	"github.com/CrimsonCoder42/red_lite_cache/protocol/rpcwire"
)

// The gRPC side of the wire (D-048): the Cache service on the client port, served by Handler; the
// Replica service is registered by internal/replica on the same server.

// errTransportClosed is Recv's error once the transport was closed or aborted.
var errTransportClosed = errors.New("rpc: transport closed")

// StreamTransport is a Transport over one gRPC stream. recv and send are the stream's own calls
// translated to JSON frames (rpcwire); the transport adds what a stream lacks: a Recv that a
// Close/Abort ends (a goroutine owns stream.Recv), and the status the stream ends with.
//
// Concurrency: send is called only from Write (Conn's writer goroutine). The receive goroutine is
// the only caller of recv; it ends when the stream's context ends, which gRPC does as soon as the
// service method returns — so every StreamTransport goroutine is gone once its RPC is.
type StreamTransport struct {
	send func(frame []byte) error
	out  chan sendItem // to the sender goroutine, the only caller of send

	in    chan recvItem
	done  chan struct{}
	abort sync.Once

	mu        sync.Mutex // guards code, st, ended, peerEnded
	code      int
	st        error
	ended     bool
	peerEnded bool // recv returned an error: the client ended its side (or the stream broke)
}

type recvItem struct {
	frame []byte
	err   error
}

type sendItem struct {
	frame []byte
	res   chan error
}

// NewStreamTransport starts the receive goroutine on ctx (the stream's context).
func NewStreamTransport(ctx context.Context, recv func() ([]byte, error), send func([]byte) error) *StreamTransport {
	t := &StreamTransport{send: send, out: make(chan sendItem), in: make(chan recvItem), done: make(chan struct{}), code: -1}
	// The sender: a Send held by flow control cannot be interrupted, and the service method must
	// return for gRPC to cancel the stream and release it. Write waits for its send or for Abort,
	// whichever is first, so Conn's writer (and so the service method) never waits on the peer.
	go func() {
		for {
			select {
			case it := <-t.out:
				it.res <- t.send(it.frame)
			case <-t.done:
				return
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		for {
			frame, err := recv()
			if err != nil {
				t.mu.Lock()
				t.peerEnded = true
				t.mu.Unlock()
			}
			select {
			case t.in <- recvItem{frame: frame, err: err}:
			case <-t.done:
				return
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	return t
}

// Recv returns the next frame, or an error once the stream or the transport ended.
func (t *StreamTransport) Recv() ([]byte, error) {
	select {
	case it := <-t.in:
		if it.err != nil {
			return nil, &CloseError{Code: t.closeCode(), Err: it.err}
		}
		return it.frame, nil
	case <-t.done:
		return nil, &CloseError{Code: t.closeCode(), Err: errTransportClosed}
	}
}

// Write sends one frame through the sender goroutine. A send held by the peer's flow control
// returns errTransportClosed as soon as Abort runs (Conn's write deadline, a full queue, a kick);
// the held send itself ends when the RPC does.
func (t *StreamTransport) Write(_ context.Context, frame []byte) error {
	res := make(chan error, 1)
	select {
	case t.out <- sendItem{frame: frame, res: res}:
	case <-t.done:
		return errTransportClosed
	}
	select {
	case err := <-res:
		return err
	case <-t.done:
		return errTransportClosed
	}
}

// Close records the status the stream ends with (the first close wins).
func (t *StreamTransport) Close(code int, reason string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.ended {
		return
	}
	t.ended, t.code, t.st = true, code, rpcwire.CloseStatus(code, reason)
}

// Abort ends Recv, so the service method returns and gRPC cancels the stream (which unblocks a
// send held by flow control).
func (t *StreamTransport) Abort() { t.abort.Do(func() { close(t.done) }) }

// Ping is nil: gRPC keepalive watches the connection (ServerOptions).
func (t *StreamTransport) Ping(context.Context) error { return nil }

// Status is what the service method returns: the recorded close status; nil when the client
// ended the stream; UNAVAILABLE (GOING_AWAY) when the server dropped it without a close (a write
// deadline), so a client never mistakes a drop for a clean end.
func (t *StreamTransport) Status() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.ended && !t.peerEnded {
		return rpcwire.CloseStatus(rpcwire.CloseGoingAway, "connection dropped")
	}
	return t.st
}

func (t *StreamTransport) closeCode() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.code
}

// ── the Cache service ───────────────────────────────────────────────────────────────────────

// CacheService implements pb.CacheServer over a Handler.
type CacheService struct {
	pb.UnimplementedCacheServer
	h          *Handler
	chunkBytes int
}

// NewCacheService serves h's sessions on the Cache service.
func NewCacheService(h *Handler) *CacheService {
	return &CacheService{h: h, chunkBytes: rpcwire.DefaultChunkBytes}
}

// Session is one client session: ClientMsgs become the router's requests; the router's frames
// become ServerMsgs (a sheet state as sorted chunks) on the one writer goroutine.
func (s *CacheService) Session(stream grpc.BidiStreamingServer[pb.ClientMsg, pb.ServerMsg]) error {
	ctx := stream.Context()
	apiKey := apiKeyFrom(ctx)
	var sheet string // written by the writer goroutine only (send), from the open reply
	t := NewStreamTransport(ctx,
		func() ([]byte, error) {
			m, err := stream.Recv()
			if err != nil {
				return nil, err
			}
			req, err := rpcwire.ClientRequest(m, apiKey)
			if err != nil {
				return nil, status.Error(codes.InvalidArgument, err.Error())
			}
			return json.Marshal(req)
		},
		func(frame []byte) error {
			msgs, err := rpcwire.ServerMsgs(frame, sheet, s.chunkBytes)
			if err != nil {
				return err
			}
			for _, m := range msgs {
				if r := m.GetReply(); r != nil && r.GetProtocolVersion() > 0 && r.GetSuccess() {
					sheet = r.GetSheet() // the open reply names the session's sheet
				}
				if err := stream.Send(m); err != nil {
					return err
				}
			}
			return nil
		})
	s.h.Serve(ctx, t, remoteOf(ctx))
	return t.Status()
}

func remoteOf(ctx context.Context) string {
	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		return p.Addr.String()
	}
	return ""
}

// ── admission interceptors ──────────────────────────────────────────────────────────────────

type ctxKey int

const keyAPIKey ctxKey = iota

// apiKeyFrom is the API key the stream interceptor read from the call metadata ("" = none).
func apiKeyFrom(ctx context.Context) string {
	k, _ := ctx.Value(keyAPIKey).(string)
	return k
}

type keyedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s keyedStream) Context() context.Context { return s.ctx }

// StreamAuth is the stream admission interceptor (D-024, D-031):
//   - Replica/Follow: the cluster token must be the call's bearer, else Unauthenticated before the
//     handler runs (no byte of the snapshot to a peer without it). An empty token = development.
//   - Cache/Session: the API key is read from `authorization: Bearer <key>` and handed to the
//     session; a malformed authorization header is refused. Whether the key admits is decided on
//     open, against the sheet it names (the router's admit), never before.
func StreamAuth(clusterToken string) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		md, _ := metadata.FromIncomingContext(ss.Context())
		switch info.FullMethod {
		case pb.Replica_Follow_FullMethodName:
			if !TokenOK(md, clusterToken) {
				return status.Error(codes.Unauthenticated, "cluster token required")
			}
			return handler(srv, ss)
		case pb.Cache_Session_FullMethodName:
			vals := md.Get(rpcwire.AuthHeader)
			if len(vals) > 0 && !strings.HasPrefix(vals[0], "Bearer ") {
				return status.Error(codes.Unauthenticated, "authorization must be a Bearer key")
			}
			ctx := context.WithValue(ss.Context(), keyAPIKey, rpcwire.BearerFromMetadata(md))
			return handler(srv, keyedStream{ServerStream: ss, ctx: ctx})
		}
		return handler(srv, ss) // the health service's Watch
	}
}

// UnaryAuth is the unary interceptor. The standard health service is open (a load balancer probes
// it without credentials); Replica/Probe checks the cluster token itself; anything else is
// refused.
func UnaryAuth() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if strings.HasPrefix(info.FullMethod, "/grpc.health.v1.Health/") || info.FullMethod == pb.Replica_Probe_FullMethodName {
			return handler(ctx, req)
		}
		return nil, status.Error(codes.Unimplemented, "no unary method")
	}
}

// TokenOK reports whether md carries token as its bearer (an empty token = development: no check).
func TokenOK(md metadata.MD, token string) bool {
	if token == "" {
		return true
	}
	return subtle.ConstantTimeCompare([]byte(rpcwire.BearerFromMetadata(md)), []byte(token)) == 1
}

// ── the server ──────────────────────────────────────────────────────────────────────────────

// ServerOptions configures NewServer.
type ServerOptions struct {
	ClusterToken  string        // REDLITE_CLUSTER_TOKEN (Replica service credentials)
	MaxRecvBytes  int           // REDLITE_MAX_FRAME_BYTES: the largest client message
	KeepaliveTime time.Duration // REDLITE_PING_INTERVAL: ping an idle connection this often
	KeepaliveWait time.Duration // REDLITE_WRITE_TIMEOUT: no ping ack within this → connection closed
	MinClientPing time.Duration // clients pinging more often than this are told GOAWAY (enforcement)
}

// NewServer builds the gRPC server of the client port: admission interceptors, keepalive with an
// enforcement policy (grpc-go Documentation/keepalive.md), the receive limit, and the standard
// health service. Services are registered by the caller (Cache here, Replica by internal/replica).
func NewServer(o ServerOptions) (*grpc.Server, *health.Server) {
	if o.MinClientPing <= 0 {
		o.MinClientPing = 5 * time.Second
	}
	srv := grpc.NewServer(
		grpc.ChainStreamInterceptor(StreamAuth(o.ClusterToken)),
		grpc.ChainUnaryInterceptor(UnaryAuth()),
		grpc.MaxRecvMsgSize(o.MaxRecvBytes),
		grpc.KeepaliveParams(keepalive.ServerParameters{Time: o.KeepaliveTime, Timeout: o.KeepaliveWait}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: o.MinClientPing, PermitWithoutStream: true}),
	)
	hs := health.NewServer()
	healthpb.RegisterHealthServer(srv, hs)
	return srv, hs
}
