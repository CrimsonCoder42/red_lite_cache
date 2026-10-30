// Covers: internal/ws admission, sessions and CloseWith
package ws

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/CrimsonCoder42/red_lite_cache/internal/lock"
	"github.com/CrimsonCoder42/red_lite_cache/internal/session"
	"github.com/CrimsonCoder42/red_lite_cache/internal/sheet"
	"github.com/CrimsonCoder42/red_lite_cache/internal/tenant"
	"github.com/CrimsonCoder42/red_lite_cache/protocol"
)

type leaderNode struct{}

func (leaderNode) IsLeader() bool      { return true }
func (leaderNode) Epoch() uint64       { return 1 }
func (leaderNode) ObserveEpoch(uint64) {}

// keys is a one-key tenant.KeyResolver.
type keys struct {
	hash           []byte
	keyID, project uuid.UUID
}

func (k keys) ResolveKey(_ context.Context, h []byte) (uuid.UUID, uuid.UUID, error) {
	if string(h) != string(k.hash) {
		return uuid.Nil, uuid.Nil, tenant.ErrUnknownKey
	}
	return k.keyID, k.project, nil
}

// recorder is a tenant.Auditor that keeps every event.
type recorder struct {
	mu sync.Mutex
	ev []tenant.Event
}

func (r *recorder) Audit(e tenant.Event) {
	r.mu.Lock()
	r.ev = append(r.ev, e)
	r.mu.Unlock()
}

func (r *recorder) events() []tenant.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]tenant.Event(nil), r.ev...)
}

// TestHandlerSessionAudit: a key-mode open is one session and one session_open event; the close
// removes it and records session_close; no event carries the key or the password.
func TestHandlerSessionAudit(t *testing.T) {
	const apiKey, password = "rlk_secret-key-material", "pw-secret"
	project := uuid.New()
	k := keys{hash: tenant.HashKey(apiKey), keyID: uuid.New(), project: project}
	reg := sheet.NewRegistry(sheet.Options{BcryptCost: 4})
	if _, err := reg.Create(project, "s1", password); err != nil {
		t.Fatal(err)
	}
	sessions := session.NewRegistry("node-1")
	rec := &recorder{}
	h := NewHandler(reg, lock.NewManager(nil), leaderNode{}, tenant.New(k, true), sessions, rec, Options{SendQueue: 8, MaxFrameBytes: 1 << 20})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := dial(t, "ws"+strings.TrimPrefix(srv.URL, "http"))

	b, _ := json.Marshal(protocol.Request{Action: protocol.ActionOpen, Sheet: "s1", Password: password, APIKey: apiKey})
	write(t, c, string(b))
	for range 2 { // reply, sheet_state
		if f, err := read(t, c); err != nil || !strings.Contains(string(f), `"success":true`) && !strings.Contains(string(f), protocol.TypeSheetState) {
			t.Fatalf("open: %s %v", f, err)
		}
	}
	l := sessions.List(session.Filter{})
	if len(l) != 1 || l[0].KeyID != k.keyID || l[0].Project != project || l[0].Node != "node-1" {
		t.Fatalf("sessions = %+v", l)
	}
	c.Close(websocket.StatusNormalClosure, "")
	deadline := time.Now().Add(5 * time.Second)
	for sessions.Len() != 0 || len(rec.events()) < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("after close: %d sessions, events %+v", sessions.Len(), rec.events())
		}
		time.Sleep(10 * time.Millisecond)
	}
	ev := rec.events()
	if ev[0].Action != "session_open" || ev[1].Action != "session_close" {
		t.Fatalf("events = %+v", ev)
	}
	for _, e := range ev {
		if e.Project != project || e.Subject != l[0].ID.String() || e.Detail["key_id"] != k.keyID.String() || e.Detail["sheet"] != "s1" || e.Detail["remote"] == "" {
			t.Fatalf("event = %+v", e)
		}
		all := e.Actor + e.Subject
		for _, v := range e.Detail {
			all += v
		}
		if strings.Contains(all, apiKey) || strings.Contains(all, password) || strings.Contains(all, string(k.hash)) {
			t.Fatalf("event carries a secret: %+v", e)
		}
	}
	if ev[1].Detail["status"] != "1000" {
		t.Fatalf("session_close status = %q, want 1000", ev[1].Detail["status"])
	}
}

// TestConnCloseWith: CloseWith sends its code to the peer; after SendAndClose it does nothing, so
// the queued final frame and its code still reach the peer.
func TestConnCloseWith(t *testing.T) {
	t.Run("code reaches the peer", func(t *testing.T) {
		url, conns := serve(t, ConnOptions{SendQueue: 4}, func(c *Conn, f []byte) {})
		a := dial(t, url)
		c := <-conns
		c.CloseWith(protocol.CloseKicked, protocol.CloseReasonKicked)
		c.CloseWith(protocol.CloseSuspended, "second call ignored")
		_, err := read(t, a)
		var ce websocket.CloseError
		if st := websocket.CloseStatus(err); st != protocol.CloseKicked {
			t.Fatalf("close status %d (%v), want %d", st, err, protocol.CloseKicked)
		}
		if errors.As(err, &ce) && ce.Reason != protocol.CloseReasonKicked {
			t.Fatalf("close reason %q", ce.Reason)
		}
		<-c.Done()
	})
	t.Run("does not drop a queued final frame", func(t *testing.T) {
		url, _ := serve(t, ConnOptions{SendQueue: 4}, func(c *Conn, f []byte) {
			c.SendAndClose([]byte("last"), websocket.StatusPolicyViolation, "bye")
			c.CloseWith(protocol.CloseKicked, protocol.CloseReasonKicked)
		})
		a := dial(t, url)
		write(t, a, "go")
		if got, err := read(t, a); err != nil || string(got) != "last" {
			t.Fatalf("got %q %v, want the final frame", got, err)
		}
		_, err := read(t, a)
		if st := websocket.CloseStatus(err); st != websocket.StatusPolicyViolation {
			t.Fatalf("close status %d (%v), want %d", st, err, websocket.StatusPolicyViolation)
		}
	})
}
