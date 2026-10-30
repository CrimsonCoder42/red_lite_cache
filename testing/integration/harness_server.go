package integration

// Server boot helpers (D-017): a real app.App on 127.0.0.1:0 per test, and readers pointed at it.
// StartReplica (wave 3) builds on Start; StartOn takes a pre-reserved listener.

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CrimsonCoder42/red_lite_cache/internal/app"
	"github.com/CrimsonCoder42/red_lite_cache/internal/config"
	"github.com/CrimsonCoder42/red_lite_cache/protocol"
	"github.com/CrimsonCoder42/red_lite_cache/reader"
)

// Server is one running instance.
type Server struct {
	Addr       string // host:port
	URL        string // http://host:port
	WSURL      string // ws://host:port/ws
	ReplicaURL string // ws://host:port/replica
	App        *app.App
	Cfg        config.Config
	// Stop cancels the instance and waits for Run to return. Idempotent; also runs at cleanup.
	Stop func()
	// Err is what Run returned; valid after Stop.
	Err func() error
}

// TestConfig is config.Default() tuned for tests: cheap bcrypt, fast loops, no jitter, short
// timeouts, and an instance id from the test name.
func TestConfig(t testing.TB) config.Config {
	cfg := config.Default()
	cfg.BcryptCost = 4
	cfg.CacheJanitorInterval = 50 * time.Millisecond
	cfg.LockCleanupInterval = 50 * time.Millisecond
	cfg.HeartbeatInterval = 100 * time.Millisecond
	cfg.HeartbeatTimeout = 500 * time.Millisecond
	cfg.PingInterval = 2 * time.Second
	cfg.WriteTimeout = 2 * time.Second
	cfg.CacheJitterPercent = 0
	cfg.ShutdownTimeout = 2 * time.Second
	cfg.InstanceID = strings.NewReplacer("/", "_", " ", "_").Replace(t.Name())
	return cfg
}

// StartLeader starts a leader from TestConfig with mods applied.
func StartLeader(t testing.TB, mods ...func(*config.Config)) *Server {
	t.Helper()
	cfg := TestConfig(t)
	cfg.Role = "leader"
	for _, m := range mods {
		m(&cfg)
	}
	return Start(t, cfg)
}

// Start runs an instance built from cfg on a fresh 127.0.0.1 port. cfg.Port is ignored.
func Start(t testing.TB, cfg config.Config) *Server {
	t.Helper()
	return StartOn(t, Listen(t), cfg)
}

// Listen reserves a 127.0.0.1 port before its server starts (a peer address or a proxy target
// must be known first). Pass it to StartOn.
func Listen(t testing.TB) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	return ln
}

// StartReplica starts a replica of leader from TestConfig (instance id <test>-replica, the leader's
// cluster token — D-031) with mods applied, and waits until it has applied the leader's snapshot.
func StartReplica(t testing.TB, leader *Server, mods ...func(*config.Config)) *Server {
	t.Helper()
	cfg := TestConfig(t)
	cfg.Role = "replica"
	cfg.LeaderAddr = leader.ReplicaURL
	cfg.ClusterToken = leader.Cfg.ClusterToken
	cfg.InstanceID += "-replica"
	for _, m := range mods {
		m(&cfg)
	}
	srv := Start(t, cfg)
	Eventually(t, "replica registered with the leader", func() bool { return leader.App.Node().Replicas() > 0 })
	return srv
}

// Health GETs srv's /health with srv's cluster token, so role and epoch are present (D-031).
func Health(t testing.TB, srv *Server) protocol.Health {
	t.Helper()
	h, err := TryHealthToken(srv.URL, srv.Cfg.ClusterToken)
	if err != nil {
		t.Fatalf("health %s: %v", srv.URL, err)
	}
	return h
}

// TryHealth GETs baseURL/health without a cluster token and decodes it.
func TryHealth(baseURL string) (protocol.Health, error) { return TryHealthToken(baseURL, "") }

// TryHealthToken GETs baseURL/health, with `Authorization: Bearer <token>` when token is set.
func TryHealthToken(baseURL, token string) (protocol.Health, error) {
	var h protocol.Health
	c := &http.Client{Timeout: 2 * time.Second}
	req, err := http.NewRequest(http.MethodGet, baseURL+protocol.PathHealth, nil)
	if err != nil {
		return h, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.Do(req)
	if err != nil {
		return h, err
	}
	defer resp.Body.Close()
	err = json.NewDecoder(resp.Body).Decode(&h)
	return h, err
}

// StartOn runs an instance built from cfg on ln (from Listen). cfg.Port is ignored.
func StartOn(t testing.TB, ln net.Listener, cfg config.Config) *Server {
	t.Helper()
	cfg.Port = ln.Addr().String()
	ctx, cancel := context.WithCancel(context.Background())
	a, err := app.New(ctx, cfg)
	if err != nil {
		cancel()
		ln.Close()
		t.Fatalf("app.New: %v", err)
	}
	done := make(chan struct{})
	var runErr error
	go func() {
		defer close(done)
		runErr = a.Run(ctx, ln)
	}()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(Timeout):
				t.Errorf("server %s did not stop within %s", cfg.Port, Timeout)
			}
		})
	}
	t.Cleanup(stop)
	addr := ln.Addr().String()
	return &Server{
		Addr: addr, URL: "http://" + addr,
		WSURL: "ws://" + addr + protocol.PathWS, ReplicaURL: "ws://" + addr + protocol.PathReplica,
		App: a, Cfg: cfg, Stop: stop,
		Err: func() error { <-done; return runErr },
	}
}

// NewReader connects a reader to srv's /ws for sheet with password "pw" and fast reconnects. It is
// closed at cleanup.
func NewReader(t testing.TB, srv *Server, sheet string, mods ...func(*reader.Config)) *reader.Reader {
	t.Helper()
	return NewReaderAt(t, srv.WSURL, "", sheet, mods...)
}

// NewReaderAt is NewReader against explicit leader and backup URLs (backup may be "").
func NewReaderAt(t testing.TB, leaderWS, backupWS, sheet string, mods ...func(*reader.Config)) *reader.Reader {
	t.Helper()
	cfg := reader.Config{
		LeaderAddr: leaderWS, BackupAddr: backupWS, Sheet: sheet, Password: "pw",
		ReconnectMin: 10 * time.Millisecond, ReconnectMax: 100 * time.Millisecond,
		ConnectTimeout: 5 * time.Second,
	}
	for _, m := range mods {
		m(&cfg)
	}
	r, err := reader.New(cfg)
	if err != nil {
		t.Fatalf("reader.New(%s): %v", sheet, err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

// OpenRaw dials srv's /ws, opens sheet with password "pw", fails the test unless it succeeds, and
// returns the conn and the sheet_state it received.
func OpenRaw(t testing.TB, srv *Server, sheet string) (*RawConn, protocol.SheetState) {
	t.Helper()
	c := Dial(t, srv.WSURL)
	resp, st := c.Open(sheet, "pw")
	if !resp.Success {
		t.Fatalf("open %s: %+v", sheet, resp)
	}
	return c, st
}

// NextType reads frames until one with type typ and decodes it into v. Other frames are skipped.
func (r *RawConn) NextType(typ string, v any) {
	r.t.Helper()
	for {
		b := r.Next()
		var env protocol.Envelope
		if err := json.Unmarshal(b, &env); err != nil {
			r.t.Fatalf("decode %s: %v", b, err)
		}
		if env.Type != typ {
			continue
		}
		if err := json.Unmarshal(b, v); err != nil {
			r.t.Fatalf("decode %s %s: %v", typ, b, err)
		}
		return
	}
}
