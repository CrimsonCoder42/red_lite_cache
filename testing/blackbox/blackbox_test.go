// Covers: the gRPC wire as a stranger sees it (D-048) — an address and the .proto, nothing else
//
// This driver imports only the code generated from proto/redlite/v1 (protocol/redlitev1), grpc and
// the standard library: no package of this server, no protocol JSON types, no rpcwire. A client in
// any language can be checked against the same steps.
//
//	REDLITE_BLACKBOX_ADDR=host:port   test a running server (password admission, autocreate on;
//	                                  REDLITE_BLACKBOX_PASSWORD, default "pw")
//	unset                             build cmd/redlite and run it in a temp directory with
//	                                  throwaway values on free ports (never the developer's .env)
package blackbox

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"

	pb "github.com/CrimsonCoder42/red_lite_cache/protocol/redlitev1"
)

var (
	addr     string
	password = "pw"
	sheetSeq atomic.Int64
)

func TestMain(m *testing.M) {
	if a := os.Getenv("REDLITE_BLACKBOX_ADDR"); a != "" {
		addr = a
		if p := os.Getenv("REDLITE_BLACKBOX_PASSWORD"); p != "" {
			password = p
		}
		os.Exit(m.Run())
	}
	stop, err := startBinary()
	if err != nil {
		fmt.Fprintln(os.Stderr, "blackbox: start server:", err)
		os.Exit(1)
	}
	code := m.Run()
	stop()
	os.Exit(code)
}

func freePort() (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer ln.Close()
	return ln.Addr().String(), nil
}

// startBinary builds cmd/redlite and runs it from an empty temp directory (no .env there) with an
// environment holding no REDLITE_* value but the throwaway ones below.
func startBinary() (func(), error) {
	dir, err := os.MkdirTemp("", "redlite-blackbox-")
	if err != nil {
		return nil, err
	}
	bin := filepath.Join(dir, "redlite")
	build := exec.Command("go", "build", "-o", bin, "../../cmd/redlite")
	if out, err := build.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("build: %v\n%s", err, out)
	}
	client, err := freePort()
	if err != nil {
		return nil, err
	}
	admin, err := freePort()
	if err != nil {
		return nil, err
	}
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "REDLITE_") {
			env = append(env, kv)
		}
	}
	env = append(env, "REDLITE_PORT="+client, "REDLITE_ADMIN_PORT="+admin, "REDLITE_ENVIRONMENT=development",
		"REDLITE_ADMISSION=password", "REDLITE_ROLE=leader", "REDLITE_LOG_LEVEL=warn",
		"REDLITE_CLUSTER_TOKEN=blackbox-throwaway", "REDLITE_BCRYPT_COST=4")
	cmd := exec.Command(bin)
	cmd.Dir, cmd.Env = dir, env
	var logs bytes.Buffer
	cmd.Stdout, cmd.Stderr = &logs, &logs
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	stop := func() {
		_ = cmd.Process.Signal(os.Interrupt)
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
		os.RemoveAll(dir)
	}
	addr = client
	if err := waitServing(20 * time.Second); err != nil {
		stop()
		return nil, fmt.Errorf("%v\n%s", err, logs.String())
	}
	return stop, nil
}

// waitServing polls the standard health service until it answers SERVING.
func waitServing(d time.Duration) error {
	cc, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer cc.Close()
	deadline := time.Now().Add(d)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		r, err := healthpb.NewHealthClient(cc).Check(ctx, &healthpb.HealthCheckRequest{})
		cancel()
		if err == nil && r.GetStatus() == healthpb.HealthCheckResponse_SERVING {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("not SERVING after %s: %v", d, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

type session struct {
	t  *testing.T
	s  grpc.BidiStreamingClient[pb.ClientMsg, pb.ServerMsg]
	in chan any
	id uint64
}

func dial(t *testing.T) *session {
	t.Helper()
	cc, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(8<<20)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); cc.Close() })
	s, err := pb.NewCacheClient(cc).Session(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ss := &session{t: t, s: s, in: make(chan any, 4096)}
	go func() {
		for {
			m, err := s.Recv()
			if err != nil {
				ss.in <- err
				return
			}
			ss.in <- m
		}
	}()
	return ss
}

func (s *session) send(m *pb.ClientMsg) uint64 {
	s.t.Helper()
	s.id++
	m.Id = s.id
	if err := s.s.Send(m); err != nil {
		s.t.Fatalf("send: %v", err)
	}
	return m.Id
}

func (s *session) next() *pb.ServerMsg {
	s.t.Helper()
	select {
	case v := <-s.in:
		if err, ok := v.(error); ok {
			s.t.Fatalf("stream ended: %v", err)
		}
		return v.(*pb.ServerMsg)
	case <-time.After(10 * time.Second):
		s.t.Fatal("no message within 10s")
	}
	return nil
}

// reply returns the Reply to request id; broadcasts read on the way are returned too.
func (s *session) reply(id uint64) (*pb.Reply, []*pb.ServerMsg) {
	s.t.Helper()
	var other []*pb.ServerMsg
	for {
		m := s.next()
		if r := m.GetReply(); r != nil && m.GetId() == id {
			return r, other
		}
		other = append(other, m)
	}
}

func (s *session) do(m *pb.ClientMsg) *pb.Reply {
	s.t.Helper()
	r, _ := s.reply(s.send(m))
	return r
}

// open opens sheet and returns its state's entries (all chunks joined).
func (s *session) open(sheet string) []*pb.Entry {
	s.t.Helper()
	r, after := s.reply(s.send(&pb.ClientMsg{Body: &pb.ClientMsg_Open{Open: &pb.Open{Sheet: sheet, Password: password, Version: 2}}}))
	if !r.GetSuccess() || r.GetSheet() != sheet || r.GetProtocolVersion() == 0 || r.GetEpoch() == 0 {
		s.t.Fatalf("open %s: %v", sheet, r)
	}
	var entries []*pb.Entry
	for _, m := range after {
		if c := m.GetState(); c != nil {
			entries = append(entries, c.GetEntries()...)
			if c.GetLast() {
				return entries
			}
		}
	}
	for {
		c := s.next().GetState()
		if c == nil {
			continue
		}
		if c.GetSheet() != sheet {
			s.t.Fatalf("state chunk for %q, want %q", c.GetSheet(), sheet)
		}
		entries = append(entries, c.GetEntries()...)
		if c.GetLast() {
			return entries
		}
	}
}

func uniqueSheet(t *testing.T) string {
	return fmt.Sprintf("bb-%s-%d-%d", strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-")), time.Now().UnixNano()%1e6, sheetSeq.Add(1))
}

func set(sheet, key, value string) *pb.ClientMsg {
	return &pb.ClientMsg{Body: &pb.ClientMsg_Set{Set: &pb.Set{Sheet: sheet, Key: key, Value: []byte(value)}}}
}

// Set, get, delete, prefix and suffix deletes, null, locks: each Reply matched by id, values
// returned as the exact bytes stored.
func TestBlackboxRequests(t *testing.T) {
	sh := uniqueSheet(t)
	s := dial(t)
	if e := s.open(sh); len(e) != 0 {
		t.Fatalf("new sheet has %d entries", len(e))
	}
	for _, k := range []string{"user:1", "user:2", "img.png"} {
		if r := s.do(set(sh, k, `{"k":"`+k+`"}`)); !r.GetSuccess() || r.GetSeq() == 0 {
			t.Fatalf("set %s: %v", k, r)
		}
	}
	if r := s.do(&pb.ClientMsg{Body: &pb.ClientMsg_Get{Get: &pb.Get{Sheet: sh, Key: "user:1"}}}); !r.GetSuccess() || string(r.GetValue()) != `{"k":"user:1"}` {
		t.Fatalf("get: %v", r)
	}
	if r := s.do(&pb.ClientMsg{Body: &pb.ClientMsg_SetNull{SetNull: &pb.SetNull{Sheet: sh, Key: "nothing"}}}); !r.GetSuccess() {
		t.Fatalf("set_null: %v", r)
	}
	if r := s.do(&pb.ClientMsg{Body: &pb.ClientMsg_DeleteSuffix{DeleteSuffix: &pb.DeleteSuffix{Sheet: sh, Suffix: ".png"}}}); !r.GetSuccess() || r.GetDeleted() != 1 {
		t.Fatalf("delete_suffix: %v", r)
	}
	if r := s.do(&pb.ClientMsg{Body: &pb.ClientMsg_DeletePrefix{DeletePrefix: &pb.DeletePrefix{Sheet: sh, Prefix: "user:"}}}); !r.GetSuccess() || r.GetDeleted() != 2 {
		t.Fatalf("delete_prefix: %v", r)
	}
	if r := s.do(&pb.ClientMsg{Body: &pb.ClientMsg_Delete{Delete: &pb.Delete{Sheet: sh, Key: "nothing"}}}); !r.GetSuccess() {
		t.Fatalf("delete: %v", r)
	}
	if r := s.do(&pb.ClientMsg{Body: &pb.ClientMsg_Get{Get: &pb.Get{Sheet: sh, Key: "user:1"}}}); r.GetSuccess() || r.GetError() == "" {
		t.Fatalf("get after delete: %v", r)
	}
	if r := s.do(&pb.ClientMsg{Body: &pb.ClientMsg_Lock{Lock: &pb.Lock{Sheet: sh, Key: "job", LockTtlSeconds: 5}}}); !r.GetSuccess() {
		t.Fatalf("lock: %v", r)
	}
	if r := s.do(&pb.ClientMsg{Body: &pb.ClientMsg_Unlock{Unlock: &pb.Unlock{Sheet: sh, Key: "job"}}}); !r.GetSuccess() {
		t.Fatalf("unlock: %v", r)
	}
	if r := s.do(&pb.ClientMsg{}); r.GetSuccess() || r.GetError() == "" {
		t.Fatalf("an empty message: %v (want a refusal, stream kept)", r)
	}
	if r := s.do(&pb.ClientMsg{Body: &pb.ClientMsg_Get{Get: &pb.Get{Sheet: sh, Key: "x"}}}); r.GetSuccess() {
		t.Fatalf("stream not usable after the refusal: %v", r)
	}
}

// A write reaches another session of the sheet as a broadcast naming the sheet, in write order;
// a later session gets the state sorted by key.
func TestBlackboxBroadcastAndState(t *testing.T) {
	sh := uniqueSheet(t)
	a, b := dial(t), dial(t)
	a.open(sh)
	b.open(sh)
	for i := range 50 {
		if r := a.do(set(sh, fmt.Sprintf("k%03d", 49-i), fmt.Sprint(i))); !r.GetSuccess() {
			t.Fatalf("set: %v", r)
		}
	}
	a.do(&pb.ClientMsg{Body: &pb.ClientMsg_Delete{Delete: &pb.Delete{Sheet: sh, Key: "k000"}}})
	for i := range 50 {
		m := b.next().GetReplicate()
		if m == nil || m.GetSheet() != sh || m.GetKey() != fmt.Sprintf("k%03d", 49-i) {
			t.Fatalf("broadcast %d = %v", i, m)
		}
	}
	if inv := b.next().GetInvalidate(); inv == nil || inv.GetSheet() != sh || inv.GetKey() != "k000" {
		t.Fatalf("invalidate = %v", inv)
	}
	entries := dial(t).open(sh)
	if len(entries) != 49 {
		t.Fatalf("state has %d entries, want 49", len(entries))
	}
	for i := 1; i < len(entries); i++ {
		if entries[i-1].GetKey() >= entries[i].GetKey() {
			t.Fatalf("state not sorted at %d: %q, %q", i, entries[i-1].GetKey(), entries[i].GetKey())
		}
	}
}

// A wrong password: a refusing Reply, then the stream ends Unauthenticated with an ErrorDetail.
func TestBlackboxWrongPassword(t *testing.T) {
	sh := uniqueSheet(t)
	dial(t).open(sh)
	s := dial(t)
	r, _ := s.reply(s.send(&pb.ClientMsg{Body: &pb.ClientMsg_Open{Open: &pb.Open{Sheet: sh, Password: password + "-wrong"}}}))
	if r.GetSuccess() {
		t.Fatal("wrong password admitted")
	}
	select {
	case v := <-s.in:
		err, ok := v.(error)
		if !ok {
			t.Fatalf("message after a refused open: %v", v)
		}
		st := status.Convert(err)
		var d *pb.ErrorDetail
		for _, x := range st.Details() {
			if e, ok := x.(*pb.ErrorDetail); ok {
				d = e
			}
		}
		if st.Code() != codes.Unauthenticated || d == nil || d.GetReason() == "" {
			t.Fatalf("ended %v detail %v", err, d)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("stream not ended after a refused open")
	}
}
