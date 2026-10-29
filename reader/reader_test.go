// Covers: reader
package reader

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"runtime"
	"testing"
	"time"

	"github.com/CrimsonCoder42/red_lite_cache/protocol"
)

var bg = context.Background()

// R1: reconnect forever — far past v1's 3+3+3 attempts — with no attempt limit.
func TestR1ReconnectForever(t *testing.T) {
	f := newFakeServer(t)
	f.put("k", `1`, 0)
	r := mustNew(t, testConfig(f.addr()))
	if !r.IsConnected() {
		t.Fatal("not connected after New")
	}
	f.setDown(true)
	waitFor(t, "disconnect", func() bool { return !r.IsConnected() })
	base := f.attemptCount()
	waitFor(t, "30 refused attempts", func() bool { return f.attemptCount()-base >= 30 })
	f.setDown(false)
	waitFor(t, "reconnect", r.IsConnected)
	var v int
	if !r.Get("k", &v) || v != 1 {
		t.Fatalf("Get after reconnect = %v", v)
	}
}

// R2: deletes while disconnected return nil, replay in order, are acked before IsConnected and
// before Get serves, and the reconnect's sheet_state does not resurrect what they deleted.
func TestR2DeletesQueuedAndReplayed(t *testing.T) {
	f := newFakeServer(t)
	for _, k := range []string{"a", "pre:1", "pre:2", "x:suf", "other"} {
		f.put(k, `"v"`, 0)
	}
	cfg := testConfig(f.addr())
	cfg.WriteTimeout = 2 * time.Second
	r := mustNew(t, cfg)
	f.setDown(true)
	waitFor(t, "disconnect", func() bool { return !r.IsConnected() })

	if err := r.Delete(bg, "a"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := r.DeletePrefix(bg, "pre:"); err != nil {
		t.Fatalf("DeletePrefix: %v", err)
	}
	if err := r.DeleteSuffix(bg, ":suf"); err != nil {
		t.Fatalf("DeleteSuffix: %v", err)
	}
	if r.localHas("a") || r.localHas("pre:1") || r.localHas("x:suf") {
		t.Fatal("deletes not applied locally first")
	}

	gate := make(chan struct{})
	f.do(func(f *fakeServer) { f.gate = gate; f.reqs = nil })
	f.setDown(false)
	waitFor(t, "replayed delete reaches the server", func() bool { return len(f.requests(protocol.ActionDelete)) == 1 })
	// Acks are held: the sheet_state (still holding the keys) is loaded, the replay is not acked.
	time.Sleep(100 * time.Millisecond)
	if r.IsConnected() {
		t.Fatal("IsConnected true before the replay was acked")
	}
	var s string
	if r.Get("other", &s) || r.Get("a", &s) {
		t.Fatal("Get served before the replay was acked")
	}
	close(gate)
	waitFor(t, "connected after replay", r.IsConnected)

	got := f.requests("")
	var order []string
	for _, q := range got {
		order = append(order, q.Action+":"+q.Key+q.Prefix+q.Suffix)
	}
	want := []string{"open:", "delete:a", "delete_prefix:pre:", "delete_suffix::suf"}
	if len(order) < 4 || order[0] != want[0] || order[1] != want[1] || order[2] != want[2] || order[3] != want[3] {
		t.Fatalf("server saw %v, want %v", order, want)
	}
	for _, k := range []string{"a", "pre:1", "pre:2", "x:suf"} {
		if r.Get(k, &s) {
			t.Fatalf("%s resurrected by sheet_state", k)
		}
	}
	if !r.Get("other", &s) || s != "v" {
		t.Fatal("unrelated key lost")
	}
}

// R2 / D-005: overflow collapses the queue to one delete_prefix "".
func TestR2QueueOverflowCollapses(t *testing.T) {
	f := newFakeServer(t)
	f.put("k", `1`, 0)
	cfg := testConfig(f.addr())
	cfg.MaxPendingDeletes = 3
	r := mustNew(t, cfg)
	f.setDown(true)
	waitFor(t, "disconnect", func() bool { return !r.IsConnected() })
	for _, k := range []string{"a", "b", "c", "d", "e"} {
		if err := r.Delete(bg, k); err != nil {
			t.Fatal(err)
		}
	}
	f.do(func(f *fakeServer) { f.reqs = nil })
	f.setDown(false)
	waitFor(t, "reconnect", r.IsConnected)
	writes := f.requests("")[1:]
	if len(writes) != 1 || writes[0].Action != protocol.ActionDeletePrefix || writes[0].Prefix != "" {
		t.Fatalf("replay = %+v, want one delete_prefix \"\"", writes)
	}
	var v int
	if r.Get("k", &v) {
		t.Fatal("k survived the whole-sheet flush")
	}
}

// R2: a delete while connected is sent and acked.
func TestDeleteConnectedAcked(t *testing.T) {
	f := newFakeServer(t)
	f.put("a", `1`, 0)
	r := mustNew(t, testConfig(f.addr()))
	if err := r.Delete(bg, "a"); err != nil {
		t.Fatal(err)
	}
	if len(f.requests(protocol.ActionDelete)) != 1 {
		t.Fatal("delete not sent")
	}
	if r.Has("a") {
		t.Fatal("a still present")
	}
}

// R3: while disconnected Get/Has/IsNull miss even though the map holds the key.
func TestR3DisconnectedReadsMiss(t *testing.T) {
	f := newFakeServer(t)
	f.put("k", `"v"`, 0)
	f.put("n", `"__NULL__"`, 0)
	r := mustNew(t, testConfig(f.addr()))
	if !r.Has("k") || !r.IsNull("n") {
		t.Fatal("not loaded")
	}
	f.setDown(true)
	waitFor(t, "disconnect", func() bool { return !r.IsConnected() })
	if !r.localHas("k") {
		t.Fatal("precondition: the map should still hold k")
	}
	var s string
	if r.Get("k", &s) || r.Has("k") || r.IsNull("n") {
		t.Fatal("read served while disconnected")
	}
	if _, ok := r.GetRaw("k"); ok {
		t.Fatal("GetRaw served while disconnected")
	}
}

// R4: a key misses after its expires_in_ms (sheet_state, replicate, set ack); MaxEntries holds.
func TestR4ExpiryAndBound(t *testing.T) {
	f := newFakeServer(t)
	f.put("st", `1`, 300)
	f.do(func(f *fakeServer) { f.setTTLms = 300 })
	r := mustNew(t, testConfig(f.addr()))
	if err := r.Set(bg, "ack", 2); err != nil {
		t.Fatal(err)
	}
	f.broadcast(protocol.Replicate{Type: protocol.TypeReplicate, Key: "rep", Value: json.RawMessage(`3`), ExpiresInMS: 300})
	waitFor(t, "replicate", func() bool { return r.Has("rep") })
	for _, k := range []string{"st", "ack", "rep"} {
		if !r.Has(k) {
			t.Fatalf("%s missing before expiry", k)
		}
	}
	time.Sleep(400 * time.Millisecond)
	for _, k := range []string{"st", "ack", "rep"} {
		if r.Has(k) {
			t.Fatalf("%s served after expiry", k)
		}
	}

	g := newFakeServer(t)
	for i := range 30 {
		g.put("s"+string(rune('a'+i)), `1`, 0)
	}
	cfg := testConfig(g.addr())
	cfg.MaxEntries = 10
	r2 := mustNew(t, cfg)
	if n := r2.local.len(); n > 10 {
		t.Fatalf("sheet_state loaded %d entries, MaxEntries 10", n)
	}
	for i := range 30 {
		g.broadcast(protocol.Replicate{Type: protocol.TypeReplicate, Key: "b" + string(rune('a'+i)), Value: json.RawMessage(`1`)})
	}
	waitFor(t, "last replicate", func() bool { return r2.Has("b" + string(rune('a'+29))) })
	if n := r2.local.len(); n > 10 {
		t.Fatalf("%d entries after broadcasts, MaxEntries 10", n)
	}
}

// R6: a set after a miss carries base_seq = the seq at the miss; without a miss, the applied seq;
// after a reconnect recorded misses send 0.
func TestR6BaseSeq(t *testing.T) {
	f := newFakeServer(t)
	f.do(func(f *fakeServer) { f.seq = 7 })
	r := mustNew(t, testConfig(f.addr()))
	var v int
	if r.Get("k", &v) {
		t.Fatal("k should miss")
	}
	f.do(func(f *fakeServer) { f.seq = 9 })
	f.broadcast(protocol.Invalidate{Type: protocol.TypeInvalidate, Key: "zz", Seq: 9})
	waitFor(t, "seq 9", func() bool { return r.local.appliedSeq() == 9 })

	if err := r.Set(bg, "k", 1); err != nil {
		t.Fatal(err)
	}
	if err := r.Set(bg, "j", 1); err != nil {
		t.Fatal(err)
	}
	if r.Has("m") {
		t.Fatal("m should miss")
	}
	sets := f.requests(protocol.ActionSet)
	if sets[0].BaseSeq == nil || *sets[0].BaseSeq != 7 {
		t.Fatalf("set k base_seq = %v, want 7", sets[0].BaseSeq)
	}
	if sets[1].BaseSeq == nil || *sets[1].BaseSeq != 10 { // seq 10 = the ack of set k
		t.Fatalf("set j base_seq = %v, want 10", sets[1].BaseSeq)
	}

	opens := len(f.requests(protocol.ActionOpen))
	f.dropAll()
	waitFor(t, "reconnect", func() bool { return len(f.requests(protocol.ActionOpen)) > opens && r.IsConnected() })
	if err := r.Set(bg, "m", 1); err != nil {
		t.Fatal(err)
	}
	sets = f.requests(protocol.ActionSet)
	if last := sets[len(sets)-1]; last.BaseSeq == nil || *last.BaseSeq != 0 {
		t.Fatalf("set m after reconnect base_seq = %v, want 0", last.BaseSeq)
	}
}

type user struct {
	Name string   `json:"name"`
	Tags []string `json:"tags"`
}

// R8: Get decodes into the caller's type; mutating it does not change the next Get; GetRaw is the
// v1 shape; the null placeholder is a Get miss.
func TestR8TypedIsolatedGet(t *testing.T) {
	f := newFakeServer(t)
	f.put("u", `{"name":"a","tags":["x"]}`, 0)
	f.put("n", `"__NULL__"`, 0)
	r := mustNew(t, testConfig(f.addr()))
	var u user
	if !r.Get("u", &u) || u.Name != "a" || len(u.Tags) != 1 {
		t.Fatalf("Get = %+v", u)
	}
	u.Name, u.Tags[0] = "mutated", "mutated"
	var u2 user
	if !r.Get("u", &u2) || u2.Name != "a" || u2.Tags[0] != "x" {
		t.Fatalf("second Get saw the mutation: %+v", u2)
	}
	raw, ok := r.GetRaw("u")
	m, isMap := raw.(map[string]any)
	if !ok || !isMap || m["name"] != "a" {
		t.Fatalf("GetRaw = %#v", raw)
	}
	var s string
	if r.Get("n", &s) {
		t.Fatal("null placeholder is a hit")
	}
	if !r.IsNull("n") || r.IsNull("u") {
		t.Fatal("IsNull wrong")
	}
}

// R9: a refused set returns an error and is not stored; a success is stored with the ack's expiry.
func TestR9AckedSet(t *testing.T) {
	f := newFakeServer(t)
	f.do(func(f *fakeServer) { f.setTTLms = 60_000 })
	cfg := testConfig(f.addr())
	cfg.WriteTimeout = 200 * time.Millisecond
	r := mustNew(t, cfg)

	before := time.Now()
	if err := r.SetWithTTL(bg, "ok", "v", 1); err != nil {
		t.Fatal(err)
	}
	e, ok := r.local.lookup("ok", time.Now())
	if !ok || e.expiresAt.Before(before.Add(59*time.Second)) || e.expiresAt.After(time.Now().Add(61*time.Second)) {
		t.Fatalf("stored = %v, expiresAt %v", ok, e.expiresAt)
	}
	if sets := f.requests(protocol.ActionSet); sets[0].TTL != 1 {
		t.Fatalf("ttl = %d", sets[0].TTL)
	}

	f.do(func(f *fakeServer) { f.setErr = protocol.ErrMsgStaleWrite })
	if err := r.Set(bg, "stale", 1); !errors.Is(err, ErrStale) {
		t.Fatalf("stale set err = %v", err)
	}
	f.do(func(f *fakeServer) { f.setErr = protocol.ErrMsgSheetFull })
	if err := r.Set(bg, "full", 1); err == nil || errors.Is(err, ErrStale) {
		t.Fatalf("sheet full err = %v", err)
	}
	f.do(func(f *fakeServer) { f.setErr = ""; f.silent = true })
	if err := r.Set(bg, "silent", 1); !errors.Is(err, ErrTimeout) {
		t.Fatalf("unacked set err = %v", err)
	}
	for _, k := range []string{"stale", "full", "silent"} {
		if r.localHas(k) {
			t.Fatalf("%s stored after failure", k)
		}
	}
	f.do(func(f *fakeServer) { f.silent = false })
	f.setDown(true)
	waitFor(t, "disconnect", func() bool { return !r.IsConnected() })
	if err := r.Set(bg, "down", 1); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("disconnected set err = %v", err)
	}
	if r.localHas("down") {
		t.Fatal("down stored")
	}
}

// R10: New against a dead server returns a reader within ConnectTimeout and connects later.
func TestR10NewWhileDown(t *testing.T) {
	f := newFakeServer(t)
	f.setDown(true)
	cfg := testConfig(f.addr())
	cfg.ConnectTimeout = 200 * time.Millisecond
	start := time.Now()
	r, err := New(cfg)
	if err != nil || r == nil {
		t.Fatalf("New = %v, %v", r, err)
	}
	t.Cleanup(func() { _ = r.Close() })
	if d := time.Since(start); d > time.Second {
		t.Fatalf("New took %v", d)
	}
	if r.IsConnected() {
		t.Fatal("connected to a dead server")
	}
	f.setDown(false)
	waitFor(t, "connect after the server appears", r.IsConnected)
}

// R10: a refused password is the one New failure; the password is never logged.
func TestAuthFailed(t *testing.T) {
	f := newFakeServer(t)
	var logs bytes.Buffer
	cfg := testConfig(f.addr())
	cfg.Password = "wrong-secret"
	cfg.Logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	start := time.Now()
	r, err := New(cfg)
	if r != nil || !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("New = %v, %v; want nil, ErrAuthFailed", r, err)
	}
	if d := time.Since(start); d >= cfg.ConnectTimeout {
		t.Fatalf("auth failure took %v", d)
	}
	if bytes.Contains(logs.Bytes(), []byte("wrong-secret")) {
		t.Fatal("password logged")
	}
}

// D-012: after seeing epoch 2, an open reply with epoch 1 is refused and the reader moves to the
// backup.
func TestEpochRefusedFailsOver(t *testing.T) {
	leader, backup := newFakeServer(t), newFakeServer(t)
	leader.do(func(f *fakeServer) { f.epoch = 2 })
	backup.do(func(f *fakeServer) { f.epoch = 2 })
	cfg := testConfig(leader.addr())
	cfg.BackupAddr = backup.addr()
	r := mustNew(t, cfg)
	if r.epoch.Load() != 2 {
		t.Fatalf("epoch = %d", r.epoch.Load())
	}
	leader.do(func(f *fakeServer) { f.epoch = 1 })
	leader.dropAll()
	waitFor(t, "connected to backup", func() bool { return backup.connCount() == 1 && r.IsConnected() })
	opens := leader.requests(protocol.ActionOpen)
	if len(opens) < 2 || opens[1].Epoch != 2 || opens[1].Version != protocol.Version {
		t.Fatalf("leader opens = %+v", opens)
	}
	if err := r.Set(bg, "k", 1); err != nil {
		t.Fatal(err)
	}
	if len(backup.requests(protocol.ActionSet)) != 1 || backup.requests(protocol.ActionSet)[0].Epoch != 2 {
		t.Fatal("write did not reach the backup with epoch 2")
	}
}

// Broadcasts: replicate, invalidate key / keys / prefix pointer / suffix pointer.
func TestBroadcastsApplied(t *testing.T) {
	f := newFakeServer(t)
	for _, k := range []string{"k", "e1", "e2", "p:1", "p:2", "1:s", "keep"} {
		f.put(k, `1`, 0)
	}
	r := mustNew(t, testConfig(f.addr()))
	f.broadcast(protocol.Replicate{Type: protocol.TypeReplicate, Key: "new", Value: json.RawMessage(`{"a":1}`), Seq: 3})
	f.broadcast(protocol.Invalidate{Type: protocol.TypeInvalidate, Key: "k", Seq: 4})
	f.broadcast(protocol.Invalidate{Type: protocol.TypeInvalidate, Keys: []string{"e1", "e2"}, Seq: 5})
	f.broadcast(protocol.Invalidate{Type: protocol.TypeInvalidate, Prefix: protocol.Ptr("p:"), Seq: 6})
	f.broadcast(protocol.Invalidate{Type: protocol.TypeInvalidate, Suffix: protocol.Ptr(":s"), Seq: 7})
	waitFor(t, "seq 7", func() bool { return r.local.appliedSeq() == 7 })
	for _, k := range []string{"k", "e1", "e2", "p:1", "p:2", "1:s"} {
		if r.Has(k) {
			t.Fatalf("%s survived its invalidate", k)
		}
	}
	if !r.Has("new") || !r.Has("keep") {
		t.Fatal("replicate or unrelated key missing")
	}
	f.broadcast(protocol.Invalidate{Type: protocol.TypeInvalidate, Prefix: protocol.Ptr(""), Seq: 8})
	waitFor(t, "prefix \"\" clears", func() bool { return r.local.len() == 0 })
}

// Close is idempotent, every call after it returns ErrClosed, and the goroutines exit.
func TestCloseIdempotentNoLeak(t *testing.T) {
	f := newFakeServer(t)
	f.put("k", `1`, 0)
	base := runtime.NumGoroutine()
	r, err := New(testConfig(f.addr()))
	if err != nil {
		t.Fatal(err)
	}
	f.dropAll()
	waitFor(t, "reconnect", func() bool { return len(f.requests(protocol.ActionOpen)) >= 2 && r.IsConnected() })
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("second Close = %v", err)
	}
	if r.IsConnected() || r.Has("k") {
		t.Fatal("reads served after Close")
	}
	if err := r.Set(bg, "k", 1); !errors.Is(err, ErrClosed) {
		t.Fatalf("Set after Close = %v", err)
	}
	for _, fn := range []func(context.Context, string) error{r.Delete, r.DeletePrefix, r.DeleteSuffix} {
		if err := fn(bg, "k"); !errors.Is(err, ErrClosed) {
			t.Fatalf("delete after Close = %v", err)
		}
	}
	waitFor(t, "goroutines to exit", func() bool { return runtime.NumGoroutine() <= base })
}

// Config.Context cancellation is Close.
func TestContextCancelCloses(t *testing.T) {
	f := newFakeServer(t)
	ctx, cancel := context.WithCancel(bg)
	cfg := testConfig(f.addr())
	cfg.Context = ctx
	r := mustNew(t, cfg)
	cancel()
	waitFor(t, "disconnect on cancel", func() bool { return !r.IsConnected() })
	if err := r.Delete(bg, "k"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Delete after cancel = %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
}

// R6 + MissWindow: a live miss record is kept by a later miss and never consumed by a set; a record
// older than MissWindow is replaced by the next miss.
func TestR6MissWindowReplaces(t *testing.T) {
	f := newFakeServer(t)
	f.do(func(f *fakeServer) { f.seq = 3 })
	cfg := testConfig(f.addr())
	cfg.MissWindow = 150 * time.Millisecond
	r := mustNew(t, cfg)
	var v int
	if r.Get("k", &v) { // record {seq 3}
		t.Fatal("k should miss")
	}
	if err := r.Set(bg, "k", 1); err != nil { // ack seq 4; record kept
		t.Fatal(err)
	}
	f.do(func(f *fakeServer) { f.seq = 9 })
	f.broadcast(protocol.Invalidate{Type: protocol.TypeInvalidate, Key: "k", Seq: 9})
	waitFor(t, "seq 9", func() bool { return r.local.appliedSeq() == 9 })
	if r.Get("k", &v) { // live record: stays 3
		t.Fatal("k should miss after invalidate")
	}
	if err := r.Set(bg, "k", 2); err != nil {
		t.Fatal(err)
	}

	time.Sleep(cfg.MissWindow + 50*time.Millisecond)
	f.do(func(f *fakeServer) { f.seq = 20 })
	f.broadcast(protocol.Invalidate{Type: protocol.TypeInvalidate, Key: "k", Seq: 20})
	waitFor(t, "seq 20", func() bool { return r.local.appliedSeq() == 20 })
	if r.Get("k", &v) { // record older than the window: replaced by {seq 20}
		t.Fatal("k should miss after the second invalidate")
	}
	if err := r.Set(bg, "k", 3); err != nil {
		t.Fatal(err)
	}
	sets := f.requests(protocol.ActionSet)
	want := []uint64{3, 3, 20}
	if len(sets) != len(want) {
		t.Fatalf("%d sets, want %d", len(sets), len(want))
	}
	for i, w := range want {
		if sets[i].BaseSeq == nil || *sets[i].BaseSeq != w {
			t.Fatalf("set %d base_seq = %v, want %d", i, sets[i].BaseSeq, w)
		}
	}
}
