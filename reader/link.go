package reader

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"

	"github.com/CrimsonCoder42/red_lite_cache/protocol"
	pb "github.com/CrimsonCoder42/red_lite_cache/protocol/redlitev1"
	"github.com/CrimsonCoder42/red_lite_cache/protocol/rpcwire"
)

// A link is one connection to a server: the gRPC Cache.Session stream at "host:port" (or
// "grpc://host:port"), D-048. It carries the JSON frames of package protocol to the session code; a
// sheet state arrives as sorted chunks and is handed over already assembled (st), so a large sheet
// is bulk-built without a JSON round trip.
type link interface {
	write(ctx context.Context, req protocol.Request) error
	read(ctx context.Context) (frame, error)
	closeNow()
}

// frame is one server message: its JSON bytes and envelope, or (a sheet state) st.
type frame struct {
	data []byte
	env  protocol.Envelope
	st   *protocol.SheetState
}

// dial opens a link to addr within ctx (the handshake deadline).
func (r *Reader) dial(ctx context.Context, addr string) (link, error) { return r.dialGRPC(ctx, addr) }

func decodeFrame(data []byte) (frame, error) {
	f := frame{data: data}
	if err := json.Unmarshal(data, &f.env); err != nil {
		return frame{}, fmt.Errorf("decode frame: %w", err)
	}
	return f, nil
}

// ── gRPC ────────────────────────────────────────────────────────────────────────────────────

// grpcLink is one Cache.Session stream on its own client connection. The API key rides in the
// call metadata (`authorization: Bearer <key>`). One goroutine receives into in; the channel holds
// one message, so a reader that stops reading stops receiving (flow control reaches the server,
// which ends the stream when its bounded queue fills).
type grpcLink struct {
	cc     *grpc.ClientConn
	stream grpc.BidiStreamingClient[pb.ClientMsg, pb.ServerMsg]
	cancel context.CancelFunc
	in     chan recvItem
	done   chan struct{}
	once   sync.Once
}

type recvItem struct {
	f   frame
	err error
}

// dialGRPC opens the stream within ctx (the handshake deadline): a connection still being set up
// when ctx ends is abandoned.
func (r *Reader) dialGRPC(ctx context.Context, addr string) (link, error) {
	cc, err := grpc.NewClient(strings.TrimPrefix(addr, "grpc://"),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithConnectParams(grpc.ConnectParams{Backoff: backoff.Config{BaseDelay: r.cfg.ReconnectMin, Multiplier: 1.6, Jitter: 0.2, MaxDelay: r.cfg.ReconnectMax}, MinConnectTimeout: r.cfg.ConnectTimeout}),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{Time: 30 * time.Second, Timeout: r.cfg.WriteTimeout, PermitWithoutStream: true}),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(readLimit)),
	)
	if err != nil {
		return nil, err
	}
	sctx, cancel := context.WithCancel(r.ctx) // the stream outlives the handshake deadline
	if r.cfg.APIKey != "" {
		sctx = metadata.AppendToOutgoingContext(sctx, rpcwire.AuthHeader, "Bearer "+r.cfg.APIKey)
	}
	stop := context.AfterFunc(ctx, cancel) // until the stream exists, the handshake deadline bounds it
	stream, err := pb.NewCacheClient(cc).Session(sctx)
	if !stop() && err == nil {
		err = ctx.Err()
	}
	if err != nil {
		cancel()
		cc.Close()
		return nil, err
	}
	l := &grpcLink{cc: cc, stream: stream, cancel: cancel, in: make(chan recvItem, 1), done: make(chan struct{})}
	go l.recvLoop()
	return l, nil
}

func (l *grpcLink) recvLoop() {
	var asm rpcwire.StateAssembler
	for {
		m, err := l.stream.Recv()
		var it recvItem
		switch {
		case err != nil:
			it.err = err
		case m.GetState() != nil:
			st, last := asm.Add(m.GetState())
			if !last {
				continue
			}
			it.f = frame{env: protocol.Envelope{Type: protocol.TypeSheetState}, st: st}
		default:
			data, ferr := rpcwire.FrameFor(m)
			if ferr == nil {
				it.f, ferr = decodeFrame(data)
			}
			it.err = ferr
		}
		select {
		case l.in <- it:
		case <-l.done:
			return
		}
		if it.err != nil {
			return
		}
	}
}

func (l *grpcLink) write(ctx context.Context, req protocol.Request) error {
	m, err := rpcwire.ClientMsgFor(req)
	if err != nil {
		return err
	}
	// Send blocks only on flow control and takes no ctx: a write still blocked when ctx ends (the
	// write deadline, or the session ending) closes the link.
	stop := context.AfterFunc(ctx, l.closeNow)
	defer stop()
	return l.stream.Send(m)
}

func (l *grpcLink) read(ctx context.Context) (frame, error) {
	select {
	case it := <-l.in:
		return it.f, it.err
	case <-ctx.Done():
		return frame{}, ctx.Err()
	case <-l.done:
		return frame{}, errLinkLost
	}
}

func (l *grpcLink) closeNow() {
	l.once.Do(func() {
		close(l.done)
		l.cancel()
		l.cc.Close()
	})
}
