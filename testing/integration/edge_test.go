// Covers: /ws routing edge cases (open gate, auth, malformed, unknown, oversize, register_replica, dump), shutdown mid-write, v1 wire compat, K9, K10, K13, K14, K15, not leader, stale epoch, /health
package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/CrimsonCoder42/red_lite_cache/internal/config"
	"github.com/CrimsonCoder42/red_lite_cache/internal/sheet"
	"github.com/CrimsonCoder42/red_lite_cache/protocol"
	"github.com/CrimsonCoder42/red_lite_cache/reader"
)

// TestEdgeRouting covers the router's refusals on one leader; each subtest is one edge case.
func TestEdgeRouting(t *testing.T) {
	srv := StartLeader(t)

	t.Run("action before open", func(t *testing.T) {
		c := Dial(t, srv.WSURL)
		for _, a := range []string{protocol.ActionGet, protocol.ActionSet, protocol.ActionLock, protocol.ActionDump, "nonsense"} {
			r := c.Do(protocol.Request{Action: a, ID: 7, Key: "k"})
			if r.Success || r.Error != protocol.ErrMsgMustOpen || r.ID != 7 {
				t.Fatalf("%s before open = %+v, want %q echoing id 7", a, r, protocol.ErrMsgMustOpen)
			}
		}
	})

	t.Run("wrong password: reply then socket closed", func(t *testing.T) {
		name := UniqueSheet(t)
		OpenRaw(t, srv, name)
		c := Dial(t, srv.WSURL)
		r := c.Do(protocol.Request{Action: protocol.ActionOpen, ID: 2, Sheet: name, Password: "wrong"})
		if r.Success || r.Error != protocol.ErrMsgInvalidPassword || r.ID != 2 {
			t.Fatalf("wrong password reply = %+v", r)
		}
		_, err := c.TryNext(5 * time.Second)
		if st := websocket.CloseStatus(err); st != websocket.StatusPolicyViolation {
			t.Fatalf("after wrong password: err %v status %d, want close %d", err, st, websocket.StatusPolicyViolation)
		}
	})

	t.Run("empty sheet or password", func(t *testing.T) {
		c := Dial(t, srv.WSURL)
		for _, req := range []protocol.Request{
			{Action: protocol.ActionOpen, Password: "pw"},
			{Action: protocol.ActionOpen, Sheet: "x"},
		} {
			if r := c.Do(req); r.Error != protocol.ErrMsgSheetAndPassword {
				t.Fatalf("open %+v = %+v", req, r)
			}
		}
		if r, _ := c.Open(UniqueSheet(t), "pw"); !r.Success {
			t.Fatalf("conn unusable after refusals: %+v", r)
		}
	})

	t.Run("malformed JSON keeps the conn", func(t *testing.T) {
		c := Dial(t, srv.WSURL)
		c.SendBytes([]byte(`{"action":"open",`))
		if r, _ := c.Reply(); r.Error != protocol.ErrMsgInvalidJSON {
			t.Fatalf("malformed = %+v", r)
		}
		c.SendBytes([]byte(`{"action":"get","key":7}`)) // wrong type is malformed too
		if r, _ := c.Reply(); r.Error != protocol.ErrMsgInvalidJSON {
			t.Fatalf("mistyped = %+v", r)
		}
		if r, _ := c.Open(UniqueSheet(t), "pw"); !r.Success {
			t.Fatalf("open after malformed: %+v", r)
		}
	})

	t.Run("unknown action, already open, key required, dump moved", func(t *testing.T) {
		c, _ := OpenRaw(t, srv, UniqueSheet(t))
		cases := []struct {
			req  protocol.Request
			want string
		}{
			{protocol.Request{Action: "frobnicate"}, protocol.ErrMsgUnknownAction},
			{protocol.Request{Action: protocol.ActionOpen, Sheet: "other", Password: "pw"}, protocol.ErrMsgAlreadyOpen},
			{protocol.Request{Action: protocol.ActionSet, Value: []byte(`1`)}, protocol.ErrMsgKeyRequired},
			{protocol.Request{Action: protocol.ActionDelete}, protocol.ErrMsgKeyRequired},
			{protocol.Request{Action: protocol.ActionLock}, protocol.ErrMsgKeyRequired},
			{protocol.Request{Action: protocol.ActionDump}, protocol.ErrMsgDumpMoved},
			{protocol.Request{Action: protocol.ActionGet, Key: "missing"}, protocol.ErrMsgKeyNotFound},
		}
		for _, tc := range cases {
			if r := c.Do(tc.req); r.Success || r.Error != tc.want {
				t.Fatalf("%+v = %+v, want %q", tc.req, r, tc.want)
			}
		}
	})

	t.Run("unlock by non-owner", func(t *testing.T) {
		name := UniqueSheet(t)
		a, _ := OpenRaw(t, srv, name)
		b, _ := OpenRaw(t, srv, name)
		if r := a.Do(lockReq("k", 0)); !r.Success {
			t.Fatalf("lock = %+v", r)
		}
		if r := b.Do(unlockReq("k")); r.Error != protocol.ErrMsgNotLockOwner {
			t.Fatalf("non-owner unlock = %+v", r)
		}
		if r := b.Do(unlockReq("never")); r.Error != protocol.ErrMsgLockNotFound {
			t.Fatalf("unlock of no lock = %+v", r)
		}
	})

	t.Run("register_replica on /ws", func(t *testing.T) {
		// A duplicate register on /replica is wave 3 (internal/replica, replica_test.go).
		c := Dial(t, srv.WSURL)
		r := c.Do(protocol.Request{Action: protocol.ActionRegisterReplica, InstanceID: "x", StartedAt: time.Now().UTC().Format(time.RFC3339)})
		if r.Error != protocol.ErrMsgUseReplicaPath {
			t.Fatalf("register_replica on /ws = %+v", r)
		}
	})

	t.Run("open reply carries epoch and protocol_version; same-epoch write accepted", func(t *testing.T) {
		c := Dial(t, srv.WSURL)
		r, _ := c.Open(UniqueSheet(t), "pw")
		if !r.Success || r.Epoch != 1 || r.ProtocolVersion != protocol.Version {
			t.Fatalf("open reply = %+v, want epoch 1 protocol_version %d", r, protocol.Version)
		}
		if r := c.Do(protocol.Request{Action: protocol.ActionSet, Key: "k", Value: []byte(`1`), Epoch: 1}); !r.Success {
			t.Fatalf("set with epoch 1 = %+v", r)
		}
	})

	t.Run("health", func(t *testing.T) {
		resp, err := http.Get(srv.URL + protocol.PathHealth)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var fields map[string]json.RawMessage
		if err := json.NewDecoder(resp.Body).Decode(&fields); err != nil {
			t.Fatal(err)
		}
		var keys []string
		for k := range fields {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/json" || !slices.Equal(keys, []string{"epoch", "instance_id", "role", "status"}) {
			t.Fatalf("health: %d %q keys %v", resp.StatusCode, resp.Header.Get("Content-Type"), keys)
		}
		var h protocol.Health
		raw, _ := json.Marshal(fields)
		json.Unmarshal(raw, &h)
		if h != (protocol.Health{Status: "ok", Role: "leader", Epoch: 1, InstanceID: srv.Cfg.InstanceID}) {
			t.Fatalf("health = %+v", h)
		}
	})
}

// TestEdgeStaleEpoch (D-012, K11): a request carrying a higher epoch than the server's is refused
// with stale epoch and its socket closed (the client must fail over) — but the claim alone never
// fences the leader: an unauthenticated frame (wrong password, unknown sheet, epoch 999999) leaves
// a standalone leader serving at its own epoch. Only the peer probe steps a leader down
// (TestFencingStepDown, TestFencingByReaderEpoch). Regression for the bootstrap review's critical
// finding: one frame used to fence a leader forever.
func TestEdgeStaleEpoch(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  func(sheet string) protocol.Request
	}{
		{"open", func(s string) protocol.Request {
			return protocol.Request{Action: protocol.ActionOpen, Sheet: s, Password: "pw", Epoch: 5}
		}},
		{"unauthenticated open", func(string) protocol.Request {
			return protocol.Request{Action: protocol.ActionOpen, Sheet: "nope", Password: "wrong", Epoch: 999999}
		}},
		{"set", func(string) protocol.Request {
			return protocol.Request{Action: protocol.ActionSet, Key: "k", Value: []byte(`1`), Epoch: 5}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := StartLeader(t)
			c := Dial(t, srv.WSURL)
			name := UniqueSheet(t)
			if tc.name == "set" {
				if r, _ := c.Open(name, "pw"); !r.Success {
					t.Fatalf("open = %+v", r)
				}
			}
			if r := c.Do(tc.req(name)); r.Error != protocol.ErrMsgStaleEpoch {
				t.Fatalf("%s with a higher epoch on an epoch-1 server = %+v", tc.name, r)
			}
			c.ExpectClosed(2 * time.Second)
			time.Sleep(3 * srv.Cfg.HeartbeatInterval) // room for any (wrong) step-down to happen
			if h := Health(t, srv); h.Role != "leader" || h.Epoch != 1 {
				t.Fatalf("health after an unverified epoch claim = %+v, want leader at epoch 1", h)
			}
			c2 := Dial(t, srv.WSURL)
			if r, _ := c2.Open(name, "pw"); !r.Success || r.Epoch != 1 {
				t.Fatalf("open after the claim = %+v, want success at epoch 1", r)
			}
		})
	}
}

// TestEdgeOversizedFrame: a frame over REDLITE_MAX_FRAME_BYTES closes the socket with 1009.
func TestEdgeOversizedFrame(t *testing.T) {
	srv := StartLeader(t, func(c *config.Config) { c.MaxFrameBytes = 1024 })
	c, _ := OpenRaw(t, srv, UniqueSheet(t))
	c.Send(protocol.Request{Action: protocol.ActionSet, Key: "k", Value: []byte(`"` + strings.Repeat("x", 4096) + `"`)})
	_, err := c.TryNext(5 * time.Second)
	if st := websocket.CloseStatus(err); st != websocket.StatusMessageTooBig {
		t.Fatalf("oversized frame: err %v status %d, want %d", err, st, websocket.StatusMessageTooBig)
	}
}

// TestEdgeV1Client: a v1 reader's raw frames (no id, no version, no epoch) get the v1 open reply,
// then sheet_state, then replicate/invalidate broadcasts it can decode with v1 field names.
func TestEdgeV1Client(t *testing.T) {
	srv := StartLeader(t)
	name := UniqueSheet(t)
	w, _ := OpenRaw(t, srv, name)
	w.Do(protocol.Request{Action: protocol.ActionSet, Key: "pre", Value: []byte(`{"a":1}`)})

	// v1 shapes, transcribed from advo_cache/reader/reader.go (response, sheet_state, broadcast)
	type v1Response struct {
		Success bool   `json:"success"`
		Sheet   string `json:"sheet,omitempty"`
		Key     string `json:"key,omitempty"`
		Error   string `json:"error,omitempty"`
	}
	type v1Broadcast struct {
		Type   string          `json:"type"`
		Key    string          `json:"key,omitempty"`
		Value  json.RawMessage `json:"value,omitempty"`
		Prefix string          `json:"prefix,omitempty"`
		Suffix string          `json:"suffix,omitempty"`
		Data   map[string]json.RawMessage
	}
	v1 := Dial(t, srv.WSURL)
	v1.SendBytes([]byte(`{"action":"open","sheet":"` + name + `","password":"pw"}`))
	var open v1Response
	if err := json.Unmarshal(v1.Next(), &open); err != nil || !open.Success || open.Sheet != name {
		t.Fatalf("v1 open reply = %+v (%v)", open, err)
	}
	var st v1Broadcast
	if err := json.Unmarshal(v1.Next(), &st); err != nil || st.Type != protocol.TypeSheetState || string(st.Data["pre"]) != `{"a":1}` {
		t.Fatalf("v1 sheet_state = %+v (%v)", st, err)
	}
	w.Do(protocol.Request{Action: protocol.ActionSet, Key: "k", Value: []byte(`[1,2]`)})
	var b v1Broadcast
	if err := json.Unmarshal(v1.Next(), &b); err != nil || b.Type != protocol.TypeReplicate || b.Key != "k" || string(b.Value) != `[1,2]` {
		t.Fatalf("v1 replicate = %+v (%v)", b, err)
	}
	w.Do(protocol.Request{Action: protocol.ActionDelete, Key: "k"})
	b = v1Broadcast{}
	if err := json.Unmarshal(v1.Next(), &b); err != nil || b.Type != protocol.TypeInvalidate || b.Key != "k" {
		t.Fatalf("v1 invalidate = %+v (%v)", b, err)
	}
	// a v1 set: no id in, none out
	v1.SendBytes([]byte(`{"action":"set","key":"v","value":"x","ttl":5,"jitter":true}`))
	raw := v1.Next()
	var r v1Response
	if err := json.Unmarshal(raw, &r); err != nil || !r.Success || r.Key != "v" || strings.Contains(string(raw), `"id"`) {
		t.Fatalf("v1 set reply = %s (%v)", raw, err)
	}
}

// TestEdgeConcurrentWritersSeq is K9 + K10: many goroutines write through two conns at once
// (-race), and every collaborator observes strictly increasing, gap-free seq.
func TestEdgeConcurrentWritersSeq(t *testing.T) {
	srv := StartLeader(t)
	name := UniqueSheet(t)
	const writers, perWriter = 4, 40
	total := 2 * writers * perWriter

	type observed struct {
		seqs []uint64
		err  error
	}
	var obs [2]observed
	var owg sync.WaitGroup
	for i := range obs {
		c, st := OpenRaw(t, srv, name)
		base := st.Seq
		owg.Go(func() {
			last := base
			for len(obs[i].seqs) < total {
				b, err := c.TryNext(Timeout)
				if err != nil {
					obs[i].err = err
					return
				}
				var rep protocol.Replicate
				if err := json.Unmarshal(b, &rep); err != nil {
					obs[i].err = err
					return
				}
				if rep.Type != protocol.TypeReplicate {
					continue
				}
				if rep.Seq != last+1 {
					obs[i].err = fmt.Errorf("seq %d after %d", rep.Seq, last)
					return
				}
				last = rep.Seq
				obs[i].seqs = append(obs[i].seqs, rep.Seq)
			}
		})
	}

	a := NewReader(t, srv, name)
	b := NewReader(t, srv, name)
	var wwg sync.WaitGroup
	var fails atomic.Int32
	for ri, r := range []*reader.Reader{a, b} {
		for w := range writers {
			wwg.Go(func() {
				for i := range perWriter {
					key := fmt.Sprintf("r%d-w%d-%d", ri, w, i%5)
					if err := r.Set(context.Background(), key, i); err != nil {
						fails.Add(1)
					}
					if i%10 == 0 {
						r.Has("shared")
					}
				}
			})
		}
	}
	wwg.Wait()
	owg.Wait()
	if n := fails.Load(); n != 0 {
		t.Fatalf("%d sets failed", n)
	}
	for i, o := range obs {
		if o.err != nil || len(o.seqs) != total {
			t.Fatalf("observer %d: %d frames, err %v", i, len(o.seqs), o.err)
		}
	}
	Eventually(t, "both readers converge", func() bool {
		var x, y int
		return a.Get("r1-w0-4", &x) && b.Get("r0-w0-4", &y)
	})
}

// TestEdgeSheetSurvivesLastLeave is K13: the sheet and its data outlive the last collaborator.
func TestEdgeSheetSurvivesLastLeave(t *testing.T) {
	srv := StartLeader(t)
	name := UniqueSheet(t)
	a, _ := OpenRaw(t, srv, name)
	a.Do(protocol.Request{Action: protocol.ActionSet, Key: "k", Value: []byte(`"v"`), TTL: 5})
	a.Conn.Close(websocket.StatusNormalClosure, "bye")
	Eventually(t, "no live conns", func() bool { return srv.App.Conns() == 0 })
	b, st := OpenRaw(t, srv, name)
	if string(st.Data["k"]) != `"v"` || st.Seq != 1 {
		t.Fatalf("reopen sheet_state = %+v", st)
	}
	if r := b.Do(protocol.Request{Action: protocol.ActionGet, Key: "k"}); string(r.Value) != `"v"` {
		t.Fatalf("get after reopen = %+v", r)
	}
	// the password is still the sheet's: a wrong one is refused
	c := Dial(t, srv.WSURL)
	if r := c.Do(protocol.Request{Action: protocol.ActionOpen, Sheet: name, Password: "other"}); r.Error != protocol.ErrMsgInvalidPassword {
		t.Fatalf("reopen with another password = %+v", r)
	}
}

// TestEdgeNoAutocreate is K14: with REDLITE_SHEET_AUTOCREATE=false an unknown sheet is refused; an
// admin-created one opens.
func TestEdgeNoAutocreate(t *testing.T) {
	srv := StartLeader(t, func(c *config.Config) { c.SheetAutocreate = false })
	name := UniqueSheet(t)
	c := Dial(t, srv.WSURL)
	if r := c.Do(protocol.Request{Action: protocol.ActionOpen, Sheet: name, Password: "pw"}); r.Error != protocol.ErrMsgSheetNotFound {
		t.Fatalf("open unknown = %+v", r)
	}
	if srv.App.Registry().Len() != 0 {
		t.Fatal("the refused open created a sheet")
	}
	if _, err := srv.App.Registry().Create(sheet.DefaultProject, name, "pw"); err != nil {
		t.Fatal(err)
	}
	if r, _ := c.Open(name, "pw"); !r.Success {
		t.Fatalf("open after Create = %+v", r)
	}
}

// TestEdgeSlowConsumerKicked is K15 (D-014): a conn that never reads fills its queue and is closed,
// while another collaborator keeps receiving promptly.
func TestEdgeSlowConsumerKicked(t *testing.T) {
	srv := StartLeader(t, func(c *config.Config) { c.ConnSendQueue = 4 })
	name := UniqueSheet(t)
	slow, _ := OpenRaw(t, srv, name) // never read until the end
	w := NewReader(t, srv, name)
	r := NewReader(t, srv, name)
	Eventually(t, "3 conns", func() bool { return srv.App.Conns() == 3 })

	pad := strings.Repeat("x", 64<<10)
	ctx := context.Background()
	var last string
	for i := 0; srv.App.Conns() == 3; i++ {
		if i == 2000 {
			t.Fatal("slow conn not kicked after 2000 x 64KB broadcasts")
		}
		last = fmt.Sprintf("%d-%s", i, pad)
		if err := w.Set(ctx, "big", last); err != nil {
			t.Fatalf("set %d: %v", i, err)
		}
	}
	// r was never kicked and receives the next write promptly
	if err := w.Set(ctx, "big", "final"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		var got string
		if r.Get("big", &got) && got == "final" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("live reader did not receive the write within 2s (connected %v)", r.IsConnected())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !r.IsConnected() || !w.IsConnected() {
		t.Fatal("a reading collaborator was dropped")
	}
	slow.ExpectClosed(Timeout)
}

// TestEdgeReplicaRefusesOpen: an instance with REDLITE_ROLE=replica answers open with "not leader"
// and reports role replica on /health.
func TestEdgeReplicaRefusesOpen(t *testing.T) {
	cfg := TestConfig(t)
	cfg.Role = "replica"
	cfg.LeaderAddr = "ws://127.0.0.1:1" + protocol.PathReplica
	cfg.HeartbeatTimeout = time.Minute // a lone replica promotes after this; keep it a replica for the test
	srv := Start(t, cfg)
	c := Dial(t, srv.WSURL)
	if r := c.Do(protocol.Request{Action: protocol.ActionOpen, Sheet: UniqueSheet(t), Password: "pw"}); r.Error != protocol.ErrMsgNotLeader {
		t.Fatalf("open on replica = %+v", r)
	}
	resp, err := http.Get(srv.URL + protocol.PathHealth)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var h protocol.Health
	json.NewDecoder(resp.Body).Decode(&h)
	if h.Role != "replica" {
		t.Fatalf("health = %+v", h)
	}
}

// TestEdgeShutdownMidWrite: readers writing in a loop while the server stops: no panic, Run
// returns nil, writes after the stop fail with an error instead of hanging, -race clean.
func TestEdgeShutdownMidWrite(t *testing.T) {
	srv := StartLeader(t)
	name := UniqueSheet(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stopped atomic.Bool
	var afterStopErrs atomic.Int32
	var wg sync.WaitGroup
	for i := range 2 {
		r := NewReader(t, srv, name, func(c *reader.Config) { c.WriteTimeout = time.Second })
		wg.Go(func() {
			for n := 0; ctx.Err() == nil; n++ {
				err := r.Set(context.Background(), fmt.Sprintf("w%d-%d", i, n%10), n)
				if err != nil && stopped.Load() {
					afterStopErrs.Add(1)
					if !errors.Is(err, reader.ErrNotConnected) && !errors.Is(err, reader.ErrTimeout) {
						t.Errorf("write after stop: unexpected error %v", err)
						return
					}
				}
				r.Delete(context.Background(), "gone") // queued while down, never an error
			}
		})
	}
	time.Sleep(200 * time.Millisecond)
	stopped.Store(true)
	srv.Stop()
	if err := srv.Err(); err != nil {
		t.Fatalf("Run returned %v on a clean stop", err)
	}
	time.Sleep(300 * time.Millisecond)
	cancel()
	wg.Wait()
	if afterStopErrs.Load() == 0 {
		t.Fatal("no write failed after the server stopped")
	}
}
