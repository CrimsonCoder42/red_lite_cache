// Covers: reader (terminal close codes, LastError, api_key — D-024, D-030)
package reader

import (
	"bytes"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/CrimsonCoder42/red_lite_cache/protocol"
)

// D-030: a server close with 4001/4002/4003 is terminal — LastError is the matching sentinel within
// 1 s, IsConnected is false, every write returns ErrClosed, Close stays idempotent, and no new
// connection is attempted for 2×ReconnectMax and beyond.
func TestTerminalCloseCodes(t *testing.T) {
	for _, tc := range []struct {
		code   int
		reason string
		want   error
	}{
		{protocol.CloseSuspended, protocol.CloseReasonSuspended, ErrSuspended},
		{protocol.CloseKeyRevoked, protocol.CloseReasonKeyRevoked, ErrAdmitted},
		{protocol.CloseKicked, protocol.CloseReasonKicked, ErrKicked},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			f := newFakeServer(t)
			f.put("k", `1`, 0)
			var logs bytes.Buffer
			cfg := testConfig(f.addr())
			cfg.Logger = slog.New(slog.NewTextHandler(&logs, nil))
			r := mustNew(t, cfg)
			if r.LastError() != nil {
				t.Fatalf("LastError before close = %v", r.LastError())
			}
			f.closeAll(tc.code, tc.reason)
			deadline := time.Now().Add(time.Second)
			for !errors.Is(r.LastError(), tc.want) {
				if time.Now().After(deadline) {
					t.Fatalf("LastError = %v after 1s, want %v", r.LastError(), tc.want)
				}
				time.Sleep(2 * time.Millisecond)
			}
			if r.LastError() != tc.want {
				t.Fatalf("LastError = %v, want the sentinel itself", r.LastError())
			}
			if r.IsConnected() || r.Has("k") {
				t.Fatal("connected or serving reads after a terminal close")
			}
			attempts := f.attemptCount()
			time.Sleep(2*cfg.ReconnectMax + 100*time.Millisecond)
			if n := f.attemptCount(); n != attempts {
				t.Fatalf("reconnected after a terminal close: %d → %d upgrade attempts", attempts, n)
			}
			if err := r.Set(bg, "k", 2); !errors.Is(err, ErrClosed) {
				t.Fatalf("Set after terminal close = %v", err)
			}
			if err := r.Delete(bg, "k"); !errors.Is(err, ErrClosed) {
				t.Fatalf("Delete after terminal close = %v", err)
			}
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			if err := r.Close(); err != nil {
				t.Fatalf("second Close = %v", err)
			}
			if r.LastError() != tc.want {
				t.Fatalf("LastError after Close = %v", r.LastError())
			}
			if !bytes.Contains(logs.Bytes(), []byte("Reader: closed by server")) {
				t.Fatalf("no closed-by-server log line:\n%s", logs.String())
			}
		})
	}
}

// R1 regression: 1000, 1001 and a dropped connection (1006) keep reconnect-forever; LastError stays nil.
func TestNonTerminalCloseReconnects(t *testing.T) {
	f := newFakeServer(t)
	r := mustNew(t, testConfig(f.addr()))
	for i, drop := range []func(){
		func() { f.closeAll(int(websocket.StatusNormalClosure), "bye") },
		func() { f.closeAll(int(websocket.StatusGoingAway), "leader stepped down") },
		f.dropAll,
	} {
		opens := len(f.requests(protocol.ActionOpen))
		drop()
		waitFor(t, "reconnect", func() bool { return len(f.requests(protocol.ActionOpen)) > opens && r.IsConnected() })
		if r.LastError() != nil {
			t.Fatalf("case %d: LastError = %v after a non-terminal close", i, r.LastError())
		}
	}
}

// D-027: an open refused "suspended" (then close 4001) is terminal — New returns ErrSuspended
// and no second attempt is made.
func TestOpenSuspendedTerminal(t *testing.T) {
	f := newFakeServer(t)
	f.do(func(f *fakeServer) { f.openErr, f.openCode = protocol.ErrMsgSuspended, protocol.CloseSuspended })
	cfg := testConfig(f.addr())
	r, err := New(cfg)
	if r != nil || !errors.Is(err, ErrSuspended) {
		t.Fatalf("New = %v, %v; want nil, ErrSuspended", r, err)
	}
	time.Sleep(2*cfg.ReconnectMax + 50*time.Millisecond)
	if n := f.attemptCount(); n != 1 {
		t.Fatalf("%d upgrade attempts after a suspended open, want 1", n)
	}
}

// D-024: "not authorized" is today's auth failure — New returns ErrAuthFailed; a reader that was
// running when its key stopped working keeps retrying with backoff (not terminal). The API key is
// sent on every open and never logged.
func TestOpenNotAuthorized(t *testing.T) {
	f := newFakeServer(t)
	f.do(func(f *fakeServer) {
		f.openErr, f.openCode = protocol.ErrMsgNotAuthorized, int(websocket.StatusPolicyViolation)
	})
	var logs bytes.Buffer
	cfg := testConfig(f.addr())
	cfg.APIKey = "rlk_secret_key_material"
	cfg.Logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	if r, err := New(cfg); r != nil || !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("New = %v, %v; want nil, ErrAuthFailed", r, err)
	}

	f.do(func(f *fakeServer) { f.openErr = "" })
	r := mustNew(t, cfg)
	f.do(func(f *fakeServer) { f.openErr = protocol.ErrMsgNotAuthorized })
	f.dropAll()
	base := f.attemptCount()
	waitFor(t, "retries after not authorized", func() bool { return f.attemptCount()-base >= 5 })
	if r.LastError() != nil || r.isClosed() {
		t.Fatalf("not authorized was treated as terminal: LastError=%v closed=%v", r.LastError(), r.isClosed())
	}
	f.do(func(f *fakeServer) { f.openErr = "" })
	waitFor(t, "reconnect once admitted again", r.IsConnected)

	opens := f.requests(protocol.ActionOpen)
	for _, o := range opens {
		if o.APIKey != cfg.APIKey {
			t.Fatalf("open without the api_key: %+v", o)
		}
	}
	_ = r.Close() // the logger buffer is read only after the reader's goroutines are gone
	if bytes.Contains(logs.Bytes(), []byte(cfg.APIKey)) {
		t.Fatal("api key logged")
	}
}

// A reader without an APIKey sends no api_key (the v1 open, D-024 password admission).
func TestNoAPIKeyOmitted(t *testing.T) {
	f := newFakeServer(t)
	mustNew(t, testConfig(f.addr()))
	if o := f.requests(protocol.ActionOpen); len(o) != 1 || o[0].APIKey != "" {
		t.Fatalf("opens = %+v", o)
	}
}
