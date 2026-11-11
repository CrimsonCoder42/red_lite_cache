// Covers: internal/rpc gRPC server over bufconn — every message, every status, slow reader, large state
package rpc

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"

	"github.com/CrimsonCoder42/red_lite_cache/internal/lock"
	"github.com/CrimsonCoder42/red_lite_cache/internal/session"
	"github.com/CrimsonCoder42/red_lite_cache/internal/sheet"
	"github.com/CrimsonCoder42/red_lite_cache/internal/tenant"
	"github.com/CrimsonCoder42/red_lite_cache/protocol"
	pb "github.com/CrimsonCoder42/red_lite_cache/protocol/redlitev1"
	"github.com/CrimsonCoder42/red_lite_cache/protocol/rpcwire"
)

const testToken = "t-cluster"

type leader struct{}

func (leader) IsLeader() bool      { return true }
func (leader) Epoch() uint64       { return 1 }
func (leader) ObserveEpoch(uint64) {}

type noKeys struct{}

func (noKeys) ResolveKey(context.Context, []byte) (uuid.UUID, uuid.UUID, error) {
	return uuid.Nil, uuid.Nil, tenant.ErrUnknownKey
}

type nopAudit struct{}

func (nopAudit) Audit(tenant.Event) {}

type bench struct {
	h        *Handler
	sessions *session.Registry
	cc       *grpc.ClientConn
	client   pb.CacheClient
}

// newBench serves a Handler (password admission, autocreate) on a bufconn gRPC server.
func newBench(t *testing.T, opts Options, maxRecv int) *bench {
	t.Helper()
	reg := sheet.NewRegistry(sheet.Options{BcryptCost: 4, Autocreate: true})
	sessions := session.NewRegistry("node-1")
	if opts.WriteTimeout == 0 {
		opts.WriteTimeout = 2 * time.Second
	}
	h := NewHandler(reg, lock.NewManager(nil), leader{}, tenant.New(noKeys{}, false), sessions, nopAudit{}, opts)
	b := benchOn(t, h, maxRecv)
	b.sessions = sessions
	return b
}

// benchFor serves h on a bufconn gRPC server (1 MiB receive limit).
func benchFor(t *testing.T, h *Handler) *bench { return benchOn(t, h, 1<<20) }

func benchOn(t *testing.T, h *Handler, maxRecv int) *bench {
	t.Helper()
	srv, hs := NewServer(ServerOptions{ClusterToken: testToken, MaxRecvBytes: maxRecv, KeepaliveTime: time.Minute, KeepaliveWait: 5 * time.Second})
	pb.RegisterCacheServer(srv, NewCacheService(h))
	pb.RegisterReplicaServer(srv, struct{ pb.UnimplementedReplicaServer }{}) // the interceptor's token check runs first
	hs.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	ln := bufconn.Listen(1 << 20)
	go srv.Serve(ln)
	cc, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return ln.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(64<<20), grpc.MaxCallSendMsgSize(64<<20)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		h.CloseAll("test over")
		cc.Close()
		srv.Stop()
	})
	return &bench{h: h, cc: cc, client: pb.NewCacheClient(cc)}
}

// stream is one Cache.Session with a receive goroutine (so a test can wait with a deadline).
type stream struct {
	t      *testing.T
	lastID uint64 // the ServerMsg id of the last reply()
	s      grpc.BidiStreamingClient[pb.ClientMsg, pb.ServerMsg]
	in     chan any // *pb.ServerMsg or error
}

func (b *bench) open(t *testing.T, sheetName string) *stream {
	t.Helper()
	s := b.stream(t, context.Background())
	s.send(&pb.ClientMsg{Id: 1, Body: &pb.ClientMsg_Open{Open: &pb.Open{Sheet: sheetName, Password: "pw", Version: protocol.Version}}})
	if r := s.reply(); !r.GetSuccess() || r.GetSheet() != sheetName {
		t.Fatalf("open %s: %+v", sheetName, r)
	}
	s.state()
	return s
}

func (b *bench) stream(t *testing.T, ctx context.Context) *stream {
	t.Helper()
	ctx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	cs, err := b.client.Session(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s := &stream{t: t, s: cs, in: make(chan any, 1<<16)}
	go func() {
		for {
			m, err := cs.Recv()
			if err != nil {
				s.in <- err
				return
			}
			s.in <- m
		}
	}()
	return s
}

func (s *stream) send(m *pb.ClientMsg) {
	s.t.Helper()
	if err := s.s.Send(m); err != nil {
		s.t.Fatalf("send: %v", err)
	}
}

func (s *stream) next() *pb.ServerMsg {
	s.t.Helper()
	select {
	case v := <-s.in:
		if err, ok := v.(error); ok {
			s.t.Fatalf("recv: %v", err)
		}
		return v.(*pb.ServerMsg)
	case <-time.After(5 * time.Second):
		s.t.Fatal("no message within 5s")
	}
	return nil
}

// end waits for the stream's status (messages before it are skipped).
func (s *stream) end() error {
	s.t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case v := <-s.in:
			if err, ok := v.(error); ok {
				return err
			}
		case <-deadline:
			s.t.Fatal("stream not ended within 10s")
		}
	}
}

func (s *stream) reply() *pb.Reply {
	s.t.Helper()
	for {
		m := s.next()
		if r := m.GetReply(); r != nil {
			s.lastID = m.GetId()
			return r
		}
	}
}

// state reads a whole chunked sheet state and returns its chunks.
func (s *stream) state() []*pb.StateChunk {
	s.t.Helper()
	var chunks []*pb.StateChunk
	for {
		c := s.next().GetState()
		if c == nil {
			continue
		}
		chunks = append(chunks, c)
		if c.GetLast() {
			return chunks
		}
	}
}

func detail(err error) *pb.ErrorDetail {
	for _, d := range status.Convert(err).Details() {
		if e, ok := d.(*pb.ErrorDetail); ok {
			return e
		}
	}
	return nil
}

// Every ClientMsg kind gets its Reply; every broadcast kind reaches a second session of the sheet,
// replies and broadcasts in one order (R2).
func TestGRPCEveryMessage(t *testing.T) {
	b := newBench(t, Options{SendQueue: 64, LockTTL: time.Second}, 1<<20)
	a := b.open(t, "s")
	w := b.open(t, "s")
	ttl := uint32(5)
	for i, tc := range []struct {
		m     *pb.ClientMsg
		check func(*pb.Reply) bool
	}{
		{&pb.ClientMsg{Body: &pb.ClientMsg_Set{Set: &pb.Set{Sheet: "s", Key: "p:1", Value: []byte(`{"v":1}`), TtlMinutes: ttl}}}, func(r *pb.Reply) bool { return r.GetSuccess() && r.GetSeq() > 0 && r.GetExpiresInMs() > 0 }},
		{&pb.ClientMsg{Body: &pb.ClientMsg_Get{Get: &pb.Get{Sheet: "s", Key: "p:1"}}}, func(r *pb.Reply) bool { return r.GetSuccess() && string(r.GetValue()) == `{"v":1}` }},
		{&pb.ClientMsg{Body: &pb.ClientMsg_SetNull{SetNull: &pb.SetNull{Sheet: "s", Key: "n:1"}}}, func(r *pb.Reply) bool { return r.GetSuccess() }},
		{&pb.ClientMsg{Body: &pb.ClientMsg_Set{Set: &pb.Set{Sheet: "s", Key: "x.a", Value: []byte(`1`)}}}, func(r *pb.Reply) bool { return r.GetSuccess() }},
		{&pb.ClientMsg{Body: &pb.ClientMsg_Delete{Delete: &pb.Delete{Sheet: "s", Key: "n:1"}}}, func(r *pb.Reply) bool { return r.GetSuccess() }},
		{&pb.ClientMsg{Body: &pb.ClientMsg_DeletePrefix{DeletePrefix: &pb.DeletePrefix{Sheet: "s", Prefix: "p:"}}}, func(r *pb.Reply) bool { return r.GetSuccess() && r.GetDeleted() == 1 }},
		{&pb.ClientMsg{Body: &pb.ClientMsg_DeleteSuffix{DeleteSuffix: &pb.DeleteSuffix{Sheet: "s", Suffix: ".a"}}}, func(r *pb.Reply) bool { return r.GetSuccess() && r.GetDeleted() == 1 }},
		{&pb.ClientMsg{Body: &pb.ClientMsg_Lock{Lock: &pb.Lock{Sheet: "s", Key: "L", LockTtlSeconds: 1}}}, func(r *pb.Reply) bool { return r.GetSuccess() }},
		{&pb.ClientMsg{Body: &pb.ClientMsg_Unlock{Unlock: &pb.Unlock{Sheet: "s", Key: "L"}}}, func(r *pb.Reply) bool { return r.GetSuccess() }},
		{rpcwire.UnknownMsg(0), func(r *pb.Reply) bool { return r.GetError() == protocol.ErrMsgUnknownAction }},
	} {
		tc.m.Id = uint64(100 + i)
		w.send(tc.m)
		r := w.reply()
		if w.lastID != tc.m.Id {
			t.Fatalf("%T: reply id %d, want %d", tc.m.GetBody(), w.lastID, tc.m.Id)
		}
		if !tc.check(r) {
			t.Fatalf("%T: reply %+v", tc.m.GetBody(), r)
		}
	}
	// a saw every write as broadcasts, in write order: replicate p:1, n:1, x.a, then the three
	// invalidations (key, prefix, suffix) — lock/unlock/get broadcast nothing.
	var kinds []string
	for len(kinds) < 6 {
		m := a.next()
		switch {
		case m.GetReplicate() != nil:
			kinds = append(kinds, "rep:"+m.GetReplicate().GetKey())
		case m.GetInvalidate() != nil:
			inv := m.GetInvalidate()
			switch {
			case inv.GetKey() != "":
				kinds = append(kinds, "inv-key:"+inv.GetKey())
			case inv.GetPrefix() != "":
				kinds = append(kinds, "inv-prefix:"+inv.GetPrefix())
			case inv.GetSuffix() != "":
				kinds = append(kinds, "inv-suffix:"+inv.GetSuffix())
			case len(inv.GetKeys().GetKeys()) > 0:
				kinds = append(kinds, "inv-keys:"+strings.Join(inv.GetKeys().GetKeys(), ","))
			}
			if inv.GetSheet() != "s" {
				t.Fatalf("invalidate without its sheet: %+v", inv)
			}
		}
	}
	got := strings.Join(kinds, " ")
	for _, want := range []string{"rep:p:1", "rep:n:1", "rep:x.a"} {
		if !strings.Contains(got, want) {
			t.Fatalf("broadcasts %q missing %q", got, want)
		}
	}
	if strings.Index(got, "rep:p:1") > strings.Index(got, "rep:x.a") || !strings.Contains(got, "inv-") {
		t.Fatalf("broadcasts out of order or no invalidation: %q", got)
	}
}

// Every way a stream ends maps to its status and ErrorDetail close code.
func TestGRPCStatusCodes(t *testing.T) {
	for _, tc := range []struct {
		code   int
		reason string
		want   codes.Code
		why    string
	}{
		{protocol.CloseSuspended, protocol.CloseReasonSuspended, codes.PermissionDenied, "SUSPENDED"},
		{protocol.CloseKeyRevoked, protocol.CloseReasonKeyRevoked, codes.Unauthenticated, "KEY_REVOKED"},
		{protocol.CloseKicked, protocol.CloseReasonKicked, codes.Aborted, "KICKED"},
		{rpcwire.ClosePolicyViolation, protocol.ErrMsgInvalidPassword, codes.Unauthenticated, "INVALID_PASSWORD"},
		{rpcwire.ClosePolicyViolation, protocol.ErrMsgNotAuthorized, codes.Unauthenticated, "NOT_AUTHORIZED"},
		{rpcwire.ClosePolicyViolation, "stale epoch", codes.FailedPrecondition, "STALE_EPOCH"},
		{rpcwire.CloseMessageTooBig, "too big", codes.ResourceExhausted, "MESSAGE_TOO_LARGE"},
		{rpcwire.CloseGoingAway, "send queue full", codes.ResourceExhausted, "SLOW_CONSUMER"},
		{rpcwire.CloseGoingAway, protocol.ErrMsgNotLeader, codes.FailedPrecondition, "NOT_LEADER"},
		{rpcwire.CloseGoingAway, "server shutting down", codes.Unavailable, "GOING_AWAY"},
	} {
		err := rpcwire.CloseStatus(tc.code, tc.reason)
		d := detail(err)
		if status.Code(err) != tc.want || d == nil || d.GetReason() != tc.why || int(d.GetCloseCode()) != tc.code {
			t.Fatalf("CloseStatus(%d, %q) = %v detail %+v; want %s %s", tc.code, tc.reason, err, d, tc.want, tc.why)
		}
		if code, _, ok := rpcwire.CloseCode(err); !ok || code != tc.code {
			t.Fatalf("CloseCode(%v) = %d %v", err, code, ok)
		}
	}
	if rpcwire.CloseStatus(rpcwire.CloseNormal, "") != nil {
		t.Fatal("a normal close is not an error")
	}

	b := newBench(t, Options{SendQueue: 64}, 1<<16)
	t.Run("wrong password", func(t *testing.T) {
		b.open(t, "pw-sheet")
		s := b.stream(t, context.Background())
		s.send(&pb.ClientMsg{Id: 2, Body: &pb.ClientMsg_Open{Open: &pb.Open{Sheet: "pw-sheet", Password: "wrong"}}})
		if r := s.reply(); r.GetSuccess() || r.GetError() != protocol.ErrMsgInvalidPassword || s.lastID != 2 {
			t.Fatalf("reply %+v", r)
		}
		if err := s.end(); status.Code(err) != codes.Unauthenticated || detail(err).GetReason() != "INVALID_PASSWORD" {
			t.Fatalf("end %v", err)
		}
	})
	t.Run("kill session", func(t *testing.T) {
		s := b.open(t, "kill")
		b.sessions.KillWhere(session.Filter{}, protocol.CloseKicked, protocol.CloseReasonKicked)
		if err := s.end(); status.Code(err) != codes.Aborted || detail(err).GetCloseCode() != protocol.CloseKicked {
			t.Fatalf("end %v", err)
		}
	})
	t.Run("kick all", func(t *testing.T) {
		s := b.open(t, "kick")
		b.h.KickAll("leader stepped down")
		if err := s.end(); status.Code(err) != codes.Unavailable || detail(err).GetCloseCode() != rpcwire.CloseGoingAway {
			t.Fatalf("end %v", err)
		}
	})
	t.Run("message too large", func(t *testing.T) {
		s := b.open(t, "big")
		s.send(&pb.ClientMsg{Id: 3, Body: &pb.ClientMsg_Set{Set: &pb.Set{Sheet: "big", Key: "k", Value: make([]byte, 1<<17)}}})
		if err := s.end(); status.Code(err) != codes.ResourceExhausted {
			t.Fatalf("end %v", err)
		}
	})
	t.Run("malformed authorization", func(t *testing.T) {
		ctx := metadata.AppendToOutgoingContext(context.Background(), rpcwire.AuthHeader, "Basic xyz")
		s := b.stream(t, ctx)
		if err := s.end(); status.Code(err) != codes.Unauthenticated {
			t.Fatalf("end %v", err)
		}
	})
	t.Run("replica without token", func(t *testing.T) {
		f, err := pb.NewReplicaClient(b.cc).Follow(context.Background())
		if err == nil {
			_, err = f.Recv()
		}
		if status.Code(err) != codes.Unauthenticated {
			t.Fatalf("Follow without token: %v", err)
		}
	})
	t.Run("health open, other unary refused", func(t *testing.T) {
		r, err := healthpb.NewHealthClient(b.cc).Check(context.Background(), &healthpb.HealthCheckRequest{})
		if err != nil || r.GetStatus() != healthpb.HealthCheckResponse_SERVING {
			t.Fatalf("health %v %v", r, err)
		}
		_, err = pb.NewReplicaClient(b.cc).Probe(context.Background(), &pb.ProbeRequest{})
		if c := status.Code(err); c != codes.Unauthenticated && c != codes.Unimplemented {
			t.Fatalf("Probe without token: %v", err)
		}
	})
}

// A reader that stops reading never stalls a broadcast: the writer's acks keep their pace, and the
// stalled stream is ended ResourceExhausted SLOW_CONSUMER once its bounded queue fills.
func TestGRPCSlowReader(t *testing.T) {
	b := newBench(t, Options{SendQueue: 16, WriteTimeout: time.Second}, 1<<20)
	slow := b.open(t, "slow")
	ok := b.open(t, "slow")
	// slow's receive goroutine keeps draining into its channel: replace it with a stalled stream
	// by opening one whose messages nobody reads at the HTTP/2 level.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stalled, err := b.client.Session(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stalled.Send(&pb.ClientMsg{Id: 1, Body: &pb.ClientMsg_Open{Open: &pb.Open{Sheet: "slow", Password: "pw"}}}); err != nil {
		t.Fatal(err)
	}
	_ = slow

	w := b.open(t, "slow")
	value := []byte(`"` + strings.Repeat("x", 4096) + `"`)
	start := time.Now()
	var worst time.Duration
	for i := range 2000 {
		t0 := time.Now()
		w.send(&pb.ClientMsg{Id: uint64(10 + i), Body: &pb.ClientMsg_Set{Set: &pb.Set{Sheet: "slow", Key: fmt.Sprintf("k%d", i), Value: value}}})
		if r := w.reply(); !r.GetSuccess() {
			t.Fatalf("set %d: %+v", i, r)
		}
		worst = max(worst, time.Since(t0))
	}
	if worst > time.Second {
		t.Fatalf("a set waited %s behind a stalled reader", worst)
	}
	// the stalled stream ended; read it now: what was sent, then SLOW_CONSUMER.
	var endErr error
	for {
		if _, err := stalled.Recv(); err != nil {
			endErr = err
			break
		}
	}
	if status.Code(endErr) != codes.ResourceExhausted || detail(endErr).GetReason() != "SLOW_CONSUMER" {
		t.Fatalf("stalled stream ended %v, want ResourceExhausted SLOW_CONSUMER", endErr)
	}
	// the reading collaborator saw every write.
	seen := 0
	for seen < 2000 {
		if m := ok.next(); m.GetReplicate() != nil {
			seen++
		}
	}
	t.Logf("2000 sets in %s, worst ack %s", time.Since(start), worst)
}

// 100k keys: the state on open arrives as sorted chunks, none over 4 MB (none over 2× the chunk
// size), the last flagged, every key exactly once.
func TestGRPCLargeState(t *testing.T) {
	b := newBench(t, Options{SendQueue: 1 << 18, WriteTimeout: 30 * time.Second}, 1<<20) // 100k pipelined under -race
	w := b.open(t, "big")
	const n = 100_000
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := range n {
			if r := w.reply(); !r.GetSuccess() {
				t.Errorf("set %d: %+v", i, r)
				return
			}
		}
	})
	for i := range n {
		w.send(&pb.ClientMsg{Id: uint64(i + 2), Body: &pb.ClientMsg_Set{Set: &pb.Set{Sheet: "big", Key: fmt.Sprintf("key:%06d", i), Value: []byte(fmt.Sprintf(`{"n":%d,"pad":"%s"}`, i, strings.Repeat("p", 32)))}}})
	}
	wg.Wait()

	r := b.stream(t, context.Background())
	r.send(&pb.ClientMsg{Id: 1, Body: &pb.ClientMsg_Open{Open: &pb.Open{Sheet: "big", Password: "pw"}}})
	r.reply()
	chunks := r.state()
	seen := make(map[string]bool, n)
	prev := ""
	for i, c := range chunks {
		size := proto.Size(&pb.ServerMsg{Body: &pb.ServerMsg_State{State: c}})
		if size >= 4<<20 || size > 2*rpcwire.DefaultChunkBytes {
			t.Fatalf("chunk %d is %d bytes", i, size)
		}
		if c.GetLast() != (i == len(chunks)-1) {
			t.Fatalf("chunk %d last=%v of %d", i, c.GetLast(), len(chunks))
		}
		if c.GetSheet() != "big" {
			t.Fatalf("chunk %d without its sheet", i)
		}
		for _, e := range c.GetEntries() {
			if e.GetKey() <= prev || seen[e.GetKey()] {
				t.Fatalf("key %q out of order or repeated (after %q)", e.GetKey(), prev)
			}
			prev = e.GetKey()
			seen[e.GetKey()] = true
		}
	}
	if len(seen) != n {
		t.Fatalf("%d keys in the state, want %d", len(seen), n)
	}
	var v map[string]any
	if err := json.Unmarshal(chunks[0].GetEntries()[0].GetValue(), &v); err != nil {
		t.Fatalf("value not the stored bytes: %v", err)
	}
	t.Logf("100k keys in %d chunks", len(chunks))
}
