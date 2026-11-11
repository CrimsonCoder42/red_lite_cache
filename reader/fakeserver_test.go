// Covers: reader
package reader

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/CrimsonCoder42/red_lite_cache/protocol"
	pb "github.com/CrimsonCoder42/red_lite_cache/protocol/redlitev1"
	"github.com/CrimsonCoder42/red_lite_cache/protocol/rpcwire"
)

// fakeServer speaks enough of the Cache service to drive the reader: open (password, epoch, seq,
// state in small chunks), acked set/delete*, broadcasts, dropping and refusing streams. It encodes
// with protocol/rpcwire, as the server does. It is not the real server.
type fakeServer struct {
	t     *testing.T
	ln    net.Listener
	chunk int // state chunk size in bytes: small, so every open exercises the assembler

	mu       sync.Mutex
	password string
	epoch    uint64
	seq      uint64
	state    map[string]json.RawMessage
	expires  map[string]int64
	refuse   bool // fail every stream at once (UNAVAILABLE)
	attempts int  // streams started
	conns    map[*fakeConn]struct{}
	reqs     []protocol.Request
	keys     []string      // the bearer of every stream
	setErr   string        // non-empty: every set is refused with this error
	setTTLms int64         // expires_in_ms on a set ack
	silent   bool          // read writes, never answer them
	gate     chan struct{} // non-nil: delete* replies wait until it is closed
	openErr  string        // non-empty: every open is refused with this error ...
	openCode int           // ... then the stream ends with this close code's status (0 = dropped)
}

// fakeConn is one stream: frames go out on send, in order, from the stream's own goroutine; end
// ends the stream with a status (nil = a normal end).
type fakeConn struct {
	send chan *pb.ServerMsg
	end  chan error
	once sync.Once
}

func (c *fakeConn) finish(err error) { c.once.Do(func() { c.end <- err }) }

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	f := &fakeServer{
		t: t, password: "pw", epoch: 1, chunk: 512,
		state: map[string]json.RawMessage{}, expires: map[string]int64{},
		conns: map[*fakeConn]struct{}{},
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f.ln = ln
	srv := grpc.NewServer()
	pb.RegisterCacheServer(srv, &fakeCache{f: f})
	go srv.Serve(ln)
	t.Cleanup(func() {
		f.dropAll()
		srv.Stop()
	})
	return f
}

func (f *fakeServer) addr() string { return f.ln.Addr().String() }

func (f *fakeServer) do(fn func(f *fakeServer)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeServer) put(key, value string, ttlMS int64) {
	f.do(func(f *fakeServer) {
		f.state[key] = json.RawMessage(value)
		if ttlMS > 0 {
			f.expires[key] = ttlMS
		}
	})
}

type fakeCache struct {
	pb.UnimplementedCacheServer
	f *fakeServer
}

func (fc *fakeCache) Session(stream grpc.BidiStreamingServer[pb.ClientMsg, pb.ServerMsg]) error {
	f := fc.f
	md, _ := metadata.FromIncomingContext(stream.Context())
	f.mu.Lock()
	f.attempts++
	f.keys = append(f.keys, rpcwire.BearerFromMetadata(md))
	refuse := f.refuse
	f.mu.Unlock()
	if refuse {
		return status.Error(codes.Unavailable, "down")
	}
	c := &fakeConn{send: make(chan *pb.ServerMsg, 1<<14), end: make(chan error, 1)}
	f.do(func(f *fakeServer) { f.conns[c] = struct{}{} })
	defer f.do(func(f *fakeServer) { delete(f.conns, c) })

	ctx := stream.Context()
	go func() {
		for {
			m, err := stream.Recv()
			if err != nil {
				c.finish(nil)
				return
			}
			req, err := rpcwire.ClientRequest(m, rpcwire.BearerFromMetadata(md))
			if err != nil {
				c.finish(status.Error(codes.InvalidArgument, err.Error()))
				return
			}
			f.do(func(f *fakeServer) { f.reqs = append(f.reqs, req) })
			if !f.serve(ctx, c, req) {
				return
			}
		}
	}()
	for {
		select {
		case m := <-c.send:
			if err := stream.Send(m); err != nil {
				return err
			}
		case err := <-c.end:
			for { // what was sent before the end goes first: one order
				select {
				case m := <-c.send:
					if stream.Send(m) != nil {
						return err
					}
					continue
				default:
				}
				return err
			}
		case <-ctx.Done():
			return nil
		}
	}
}

func (f *fakeServer) write(c *fakeConn, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		f.t.Errorf("fake: encode: %v", err)
		return
	}
	msgs, err := rpcwire.ServerMsgs(data, "s", f.chunk)
	if err != nil {
		f.t.Errorf("fake: %s: %v", data, err)
		return
	}
	for _, m := range msgs {
		select {
		case c.send <- m:
		default: // a stalled fake stream: drop, as a kicked one would
		}
	}
}

// dropped is how the fake ends a stream abruptly (UNAVAILABLE, no close code).
func dropped() error { return status.Error(codes.Unavailable, "dropped") }

func (f *fakeServer) serve(ctx context.Context, c *fakeConn, req protocol.Request) bool {
	switch req.Action {
	case protocol.ActionOpen:
		f.mu.Lock()
		if f.openErr != "" {
			e, code := f.openErr, f.openCode
			f.mu.Unlock()
			f.write(c, protocol.Response{ID: req.ID, Error: e})
			if code != 0 {
				c.finish(rpcwire.CloseStatus(code, e))
			} else {
				c.finish(dropped())
			}
			return false
		}
		if req.Password != f.password {
			f.mu.Unlock()
			f.write(c, protocol.Response{ID: req.ID, Error: protocol.ErrMsgInvalidPassword})
			c.finish(dropped())
			return false
		}
		reply := protocol.Response{Success: true, ID: req.ID, Sheet: req.Sheet, ProtocolVersion: protocol.Version, Epoch: f.epoch, Seq: f.seq}
		st := protocol.SheetState{Type: protocol.TypeSheetState, Data: map[string]json.RawMessage{}, ExpiresInMS: map[string]int64{}, Seq: f.seq, Epoch: f.epoch}
		for k, v := range f.state {
			st.Data[k] = v
		}
		for k, v := range f.expires {
			st.ExpiresInMS[k] = v
		}
		f.mu.Unlock()
		f.write(c, reply)
		f.write(c, st)
		return true

	case protocol.ActionSet:
		f.mu.Lock()
		if f.silent {
			f.mu.Unlock()
			return true
		}
		if f.setErr != "" {
			e := f.setErr
			f.mu.Unlock()
			f.write(c, protocol.Response{ID: req.ID, Error: e})
			return true
		}
		f.state[req.Key] = req.Value
		f.seq++
		reply := protocol.Response{Success: true, ID: req.ID, Key: req.Key, Seq: f.seq, ExpiresInMS: f.setTTLms}
		f.mu.Unlock()
		f.write(c, reply)
		return true

	case protocol.ActionDelete, protocol.ActionDeletePrefix, protocol.ActionDeleteSuffix:
		f.mu.Lock()
		gate, silent := f.gate, f.silent
		f.mu.Unlock()
		if silent {
			return true
		}
		if gate != nil {
			select {
			case <-gate:
			case <-ctx.Done():
				return false
			}
		}
		f.mu.Lock()
		n := 0
		for k := range f.state {
			hit := false
			switch req.Action {
			case protocol.ActionDelete:
				hit = k == req.Key
			case protocol.ActionDeletePrefix:
				hit = strings.HasPrefix(k, req.Prefix)
			default:
				hit = strings.HasSuffix(k, req.Suffix)
			}
			if hit {
				delete(f.state, k)
				n++
			}
		}
		f.seq++
		reply := protocol.Response{Success: true, ID: req.ID, Key: req.Key, Deleted: n, Seq: f.seq}
		f.mu.Unlock()
		f.write(c, reply)
		return true
	}
	f.write(c, protocol.Response{ID: req.ID, Error: protocol.ErrMsgUnknownAction})
	return true
}

func (f *fakeServer) each(fn func(c *fakeConn)) {
	f.mu.Lock()
	conns := make([]*fakeConn, 0, len(f.conns))
	for c := range f.conns {
		conns = append(conns, c)
	}
	f.mu.Unlock()
	for _, c := range conns {
		fn(c)
	}
}

// broadcast sends v to every open stream.
func (f *fakeServer) broadcast(v any) { f.each(func(c *fakeConn) { f.write(c, v) }) }

// dropAll ends every stream abruptly (UNAVAILABLE, no close code).
func (f *fakeServer) dropAll() { f.each(func(c *fakeConn) { c.finish(dropped()) }) }

// closeAll ends every stream with a close code's status (D-030 codes are terminal).
func (f *fakeServer) closeAll(code int, reason string) {
	f.each(func(c *fakeConn) { c.finish(rpcwire.CloseStatus(code, reason)) })
}

// setDown refuses (and drops) streams while down.
func (f *fakeServer) setDown(down bool) {
	f.do(func(f *fakeServer) { f.refuse = down })
	if down {
		f.dropAll()
	}
}

func (f *fakeServer) requests(action string) []protocol.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []protocol.Request
	for _, r := range f.reqs {
		if action == "" || r.Action == action {
			out = append(out, r)
		}
	}
	return out
}

func (f *fakeServer) attemptCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attempts
}

func (f *fakeServer) connCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.conns)
}

// testConfig is a fast config against addr: millisecond backoff, short timeouts, a silent logger.
func testConfig(addr string) Config {
	return Config{
		LeaderAddr: addr, Sheet: "s", Password: "pw",
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		ConnectTimeout: 2 * time.Second, WriteTimeout: time.Second,
		ReconnectMin: time.Millisecond, ReconnectMax: 10 * time.Millisecond,
	}
}

func mustNew(t *testing.T, cfg Config) *Reader {
	t.Helper()
	r, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// localHas reports whether the local map holds key, ignoring the connected gate.
func (r *Reader) localHas(key string) bool {
	_, ok := r.local.lookup(key, time.Now())
	return ok
}
