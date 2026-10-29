// Covers: reader
package reader

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/CrimsonCoder42/red_lite_cache/protocol"
)

// fakeServer speaks enough of protocol/ to drive the reader: open (password, epoch, seq, state),
// acked set/delete*, broadcasts, dropping and refusing connections. It is not the real server.
type fakeServer struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	password string
	epoch    uint64
	seq      uint64
	state    map[string]json.RawMessage
	expires  map[string]int64
	refuse   bool // answer 503 to every upgrade
	attempts int  // upgrade requests seen
	conns    map[*websocket.Conn]struct{}
	reqs     []protocol.Request
	setErr   string        // non-empty: every set is refused with this error
	setTTLms int64         // expires_in_ms on a set ack
	silent   bool          // read writes, never answer them
	gate     chan struct{} // non-nil: delete* replies wait until it is closed
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	f := &fakeServer{
		t: t, password: "pw", epoch: 1,
		state: map[string]json.RawMessage{}, expires: map[string]int64{},
		conns: map[*websocket.Conn]struct{}{},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(func() {
		f.dropAll()
		f.srv.Close()
	})
	return f
}

func (f *fakeServer) addr() string {
	return "ws" + strings.TrimPrefix(f.srv.URL, "http") + protocol.PathWS
}

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

func (f *fakeServer) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.attempts++
	refuse := f.refuse
	f.mu.Unlock()
	if refuse {
		http.Error(w, "down", http.StatusServiceUnavailable)
		return
	}
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	c.SetReadLimit(64 << 20)
	f.do(func(f *fakeServer) { f.conns[c] = struct{}{} })
	defer func() {
		f.do(func(f *fakeServer) { delete(f.conns, c) })
		_ = c.CloseNow()
	}()
	ctx := r.Context()
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		var req protocol.Request
		if err := json.Unmarshal(data, &req); err != nil {
			return
		}
		f.do(func(f *fakeServer) { f.reqs = append(f.reqs, req) })
		if !f.serve(ctx, c, req) {
			return
		}
	}
}

func (f *fakeServer) write(ctx context.Context, c *websocket.Conn, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		f.t.Errorf("fake: encode: %v", err)
		return
	}
	wctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	_ = c.Write(wctx, websocket.MessageText, data)
}

func (f *fakeServer) serve(ctx context.Context, c *websocket.Conn, req protocol.Request) bool {
	switch req.Action {
	case protocol.ActionOpen:
		f.mu.Lock()
		if req.Password != f.password {
			f.mu.Unlock()
			f.write(ctx, c, protocol.Response{ID: req.ID, Error: protocol.ErrMsgInvalidPassword})
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
		f.write(ctx, c, reply)
		f.write(ctx, c, st)
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
			f.write(ctx, c, protocol.Response{ID: req.ID, Error: e})
			return true
		}
		f.state[req.Key] = req.Value
		f.seq++
		reply := protocol.Response{Success: true, ID: req.ID, Key: req.Key, Seq: f.seq, ExpiresInMS: f.setTTLms}
		f.mu.Unlock()
		f.write(ctx, c, reply)
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
		f.write(ctx, c, reply)
		return true
	}
	f.write(ctx, c, protocol.Response{ID: req.ID, Error: protocol.ErrMsgUnknownAction})
	return true
}

// broadcast sends v to every open connection.
func (f *fakeServer) broadcast(v any) {
	f.mu.Lock()
	conns := make([]*websocket.Conn, 0, len(f.conns))
	for c := range f.conns {
		conns = append(conns, c)
	}
	f.mu.Unlock()
	for _, c := range conns {
		f.write(context.Background(), c, v)
	}
}

func (f *fakeServer) dropAll() {
	f.mu.Lock()
	conns := make([]*websocket.Conn, 0, len(f.conns))
	for c := range f.conns {
		conns = append(conns, c)
	}
	f.mu.Unlock()
	for _, c := range conns {
		_ = c.CloseNow()
	}
}

// setDown refuses (and drops) connections while down.
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
