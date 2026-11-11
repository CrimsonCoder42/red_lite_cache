package integration

// RawConn: a message-level client for edge-case tests over Cache.Session (D-048). Tests speak
// protocol.Request and read JSON frames of package protocol: messages are translated by
// protocol/rpcwire (the one translation the server and the reader use too), a sheet state's chunks
// are joined into one sheet_state frame, and a stream status becomes a read error whose close code
// CloseStatus reads.

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	"github.com/CrimsonCoder42/red_lite_cache/protocol"
	pb "github.com/CrimsonCoder42/red_lite_cache/protocol/redlitev1"
	"github.com/CrimsonCoder42/red_lite_cache/protocol/rpcwire"
)

// Close codes a test asserts (the ErrorDetail close_code, rpcwire; D-030 codes in protocol).
const (
	CloseNormal          = rpcwire.CloseNormal
	CloseGoingAway       = rpcwire.CloseGoingAway
	ClosePolicyViolation = rpcwire.ClosePolicyViolation
	CloseMessageTooBig   = rpcwire.CloseMessageTooBig
)

// CloseStatus is the close code a read error carries: the stream status's ErrorDetail close_code
// (rpcwire.CloseCode); -1 when there is none.
func CloseStatus(err error) int {
	if code, _, ok := rpcwire.CloseCode(err); ok {
		return code
	}
	return -1
}

// RawConn sends protocol.Request frames and reads whatever the server sends, with no reader logic
// in between.
type RawConn struct {
	t testing.TB
	l rawLink
}

type rawLink interface {
	write(ctx context.Context, req *protocol.Request, raw []byte) error
	read(ctx context.Context) ([]byte, error)
	closeNormal()
	closeNow()
	stopReading()
}

// Dial opens a raw Cache.Session to url (host:port) and closes it at cleanup.
func Dial(t testing.TB, url string) *RawConn {
	t.Helper()
	r, err := TryDial(t, url)
	if err != nil {
		t.Fatalf("dial %s: %v", url, err)
	}
	return r
}

// TryDial is Dial returning the error instead of failing (safe off the test goroutine).
func TryDial(t testing.TB, url string) (*RawConn, error) {
	cc, err := grpc.NewClient(url, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, err
	}
	l := &grpcRaw{cc: cc, in: make(chan rawItem), done: make(chan struct{})} // unbuffered: like a socket nobody reads, a peer the test does not read stalls
	t.Cleanup(l.closeNow)
	return &RawConn{t: t, l: l}, nil
}

// TrySend is Send returning the error instead of failing (safe off the test goroutine).
func (r *RawConn) TrySend(req protocol.Request) error {
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), Timeout)
	defer cancel()
	return r.l.write(ctx, &req, b)
}

// Send writes req as one frame.
func (r *RawConn) Send(req protocol.Request) {
	r.t.Helper()
	b, err := json.Marshal(req)
	if err != nil {
		r.t.Fatalf("marshal: %v", err)
	}
	r.write(&req, b)
}

// SendBytes writes b, a JSON protocol.Request, as one message (a hand-written request).
func (r *RawConn) SendBytes(b []byte) {
	r.t.Helper()
	var req protocol.Request
	if err := json.Unmarshal(b, &req); err != nil {
		r.t.Fatalf("SendBytes: %v", err)
	}
	r.write(&req, b)
}

func (r *RawConn) write(req *protocol.Request, raw []byte) {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), Timeout)
	defer cancel()
	if err := r.l.write(ctx, req, raw); err != nil {
		r.t.Fatalf("write: %v", err)
	}
}

// Next reads the next frame, failing the test after Timeout.
func (r *RawConn) Next() []byte {
	r.t.Helper()
	b, err := r.TryNext(Timeout)
	if err != nil {
		r.t.Fatalf("read: %v", err)
	}
	return b
}

// TryNext reads the next frame or returns the error (a closed connection, or a timeout after d:
// context.DeadlineExceeded).
func (r *RawConn) TryNext(d time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	return r.l.read(ctx)
}

// CloseNormal ends the stream from the client side and drains to the server's end.
func (r *RawConn) CloseNormal() { r.l.closeNormal() }

// CloseNow drops the connection at once.
func (r *RawConn) CloseNow() { r.l.closeNow() }

// StopReading stops delivering frames to the test: they are read and discarded in the background,
// so the connection still sees its end. Not a stalled reader.
func (r *RawConn) StopReading() { r.l.stopReading() }

// Reply reads frames until one without "type" (a direct reply) and decodes it. Broadcasts read on
// the way are returned too, in order.
func (r *RawConn) Reply() (protocol.Response, [][]byte) {
	r.t.Helper()
	var skipped [][]byte
	for {
		b := r.Next()
		var env protocol.Envelope
		if err := json.Unmarshal(b, &env); err != nil {
			r.t.Fatalf("decode %s: %v", b, err)
		}
		if env.Type != "" {
			skipped = append(skipped, b)
			continue
		}
		var resp protocol.Response
		if err := json.Unmarshal(b, &resp); err != nil {
			r.t.Fatalf("decode reply %s: %v", b, err)
		}
		return resp, skipped
	}
}

// Do sends req and returns its reply.
func (r *RawConn) Do(req protocol.Request) protocol.Response {
	r.t.Helper()
	r.Send(req)
	resp, _ := r.Reply()
	return resp
}

// Open sends open and returns the reply and the sheet_state frame that follows a success.
func (r *RawConn) Open(sheet, password string) (protocol.Response, protocol.SheetState) {
	r.t.Helper()
	resp := r.Do(protocol.Request{Action: protocol.ActionOpen, Sheet: sheet, Password: password})
	var st protocol.SheetState
	if !resp.Success {
		return resp, st
	}
	if err := json.Unmarshal(r.Next(), &st); err != nil || st.Type != protocol.TypeSheetState {
		r.t.Fatalf("expected sheet_state after open, got %+v (err %v)", st, err)
	}
	return resp, st
}

// ExpectClosed fails unless the server closes the connection within d.
func (r *RawConn) ExpectClosed(d time.Duration) {
	r.t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		_, err := r.TryNext(time.Until(deadline))
		if err == nil {
			continue // drain frames sent before the close
		}
		if errors.Is(err, context.DeadlineExceeded) {
			break
		}
		return
	}
	r.t.Fatalf("connection still open after %s", d)
}

// ── gRPC ────────────────────────────────────────────────────────────────────────────────────

type rawItem struct {
	frame []byte
	err   error
}

// grpcRaw is one Cache.Session stream on its own client connection. The stream starts on the first
// write, so an open's API key can ride in the call metadata. One goroutine receives into in
// (discarding after StopReading), joining a sheet state's chunks into one sheet_state frame; when
// the stream ends the client connection is closed too.
type grpcRaw struct {
	cc      *grpc.ClientConn
	mu      sync.Mutex
	stream  grpc.BidiStreamingClient[pb.ClientMsg, pb.ServerMsg]
	cancel  context.CancelFunc
	in      chan rawItem
	discard bool
	done    chan struct{}
	once    sync.Once
}

func (g *grpcRaw) start(apiKey string) error {
	ctx, cancel := context.WithCancel(context.Background())
	if apiKey != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, rpcwire.AuthHeader, "Bearer "+apiKey)
	}
	s, err := pb.NewCacheClient(g.cc).Session(ctx)
	if err != nil {
		cancel()
		return err
	}
	g.stream, g.cancel = s, cancel
	go func() {
		var asm rpcwire.StateAssembler
		defer g.closeNow()
		for {
			m, err := s.Recv()
			g.mu.Lock()
			discard := g.discard
			g.mu.Unlock()
			if err != nil {
				if !discard {
					g.deliver(rawItem{err: err})
				}
				return
			}
			if discard {
				continue
			}
			var frame []byte
			if c := m.GetState(); c != nil {
				st, done := asm.Add(c)
				if !done {
					continue
				}
				frame, err = json.Marshal(st)
			} else {
				frame, err = rpcwire.FrameFor(m)
			}
			g.deliver(rawItem{frame: frame, err: err})
			if err != nil {
				return
			}
		}
	}()
	return nil
}

func (g *grpcRaw) deliver(it rawItem) {
	select {
	case g.in <- it:
	case <-g.done:
	}
}

func (g *grpcRaw) write(_ context.Context, req *protocol.Request, _ []byte) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.stream == nil {
		if err := g.start(req.APIKey); err != nil {
			return err
		}
	}
	m, err := rpcwire.ClientMsgFor(*req)
	if err != nil { // dump, or an unknown action: no message on gRPC — sent as an empty body
		m = rpcwire.UnknownMsg(req.ID)
	}
	return g.stream.Send(m)
}

func (g *grpcRaw) read(ctx context.Context) ([]byte, error) {
	select {
	case it := <-g.in: // what arrived before the end, the end's status included, goes first
		return it.frame, it.err
	default:
	}
	select {
	case it := <-g.in:
		return it.frame, it.err
	case <-g.done:
		return nil, errRawClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

var errRawClosed = errors.New("raw connection closed")

// closeNormal ends the client's side and drains to the server's end, which frees the client
// connection.
func (g *grpcRaw) closeNormal() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.discard = true
	if g.stream == nil {
		go g.closeNow()
		return
	}
	_ = g.stream.CloseSend()
}

func (g *grpcRaw) closeNow() {
	g.once.Do(func() {
		close(g.done)
		g.mu.Lock()
		if g.cancel != nil {
			g.cancel()
		}
		g.mu.Unlock()
		g.cc.Close()
	})
}

func (g *grpcRaw) stopReading() {
	g.mu.Lock()
	g.discard = true
	g.mu.Unlock()
}

// DialReplica opens Replica/Follow to srv with srv's cluster token and closes it at cleanup. Send register_replica on it; frames read are the JSON
// replica frames of package protocol (a snapshot sheet's chunks joined).
func DialReplica(t testing.TB, srv *Server) *RawConn {
	t.Helper()
	cc, err := grpc.NewClient(srv.Addr, grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(64<<20)))
	if err != nil {
		t.Fatalf("dial %s: %v", srv.Addr, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if tok := srv.Cfg.ClusterToken; tok != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, rpcwire.AuthHeader, "Bearer "+tok)
	}
	s, err := pb.NewReplicaClient(cc).Follow(ctx)
	if err != nil {
		cancel()
		cc.Close()
		t.Fatalf("follow %s: %v", srv.Addr, err)
	}
	l := &grpcReplicaRaw{cc: cc, s: s, cancel: cancel, in: make(chan rawItem, 4096)}
	go func() {
		var asm rpcwire.SnapshotAssembler
		defer l.closeNow() // the stream ended: free the client connection too
		for {
			m, err := s.Recv()
			if err != nil {
				l.in <- rawItem{err: err}
				return
			}
			frame, err := rpcwire.ReplicaFrameFor(m, &asm)
			if err == nil && frame == nil {
				continue // a snapshot chunk that is not its sheet's last
			}
			l.in <- rawItem{frame: frame, err: err}
			if err != nil {
				return
			}
		}
	}()
	t.Cleanup(l.closeNow)
	return &RawConn{t: t, l: l}
}

type grpcReplicaRaw struct {
	cc     *grpc.ClientConn
	s      grpc.BidiStreamingClient[pb.ReplicaClientMsg, pb.ReplicaServerMsg]
	cancel context.CancelFunc
	in     chan rawItem
	once   sync.Once
}

func (g *grpcReplicaRaw) write(_ context.Context, req *protocol.Request, _ []byte) error {
	return g.s.Send(rpcwire.RegisterMsg(*req))
}

func (g *grpcReplicaRaw) read(ctx context.Context) ([]byte, error) {
	select {
	case it := <-g.in:
		return it.frame, it.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (g *grpcReplicaRaw) closeNormal() { _ = g.s.CloseSend() }
func (g *grpcReplicaRaw) stopReading() {}
func (g *grpcReplicaRaw) closeNow()    { g.once.Do(func() { g.cancel(); g.cc.Close() }) }
