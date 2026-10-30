// Covers: reader terminal close codes against a real leader (4001/4002/4003, LastError — D-030, tenancy criterion 5 reader half)
package integration

import (
	"errors"
	"testing"
	"time"

	"github.com/CrimsonCoder42/red_lite_cache/internal/session"
	"github.com/CrimsonCoder42/red_lite_cache/protocol"
	"github.com/CrimsonCoder42/red_lite_cache/reader"
)

// TestTerminalCloseReader: the leader kills the reader's session with each terminal code; the
// reader's LastError is the matching sentinel within 1 s, it is not connected, writes return
// ErrClosed, and it never opens again (no session, no conn for 3× ReconnectMax).
func TestTerminalCloseReader(t *testing.T) {
	srv := StartLeader(t)
	const reconnectMax = 100 * time.Millisecond // NewReaderAt's
	for _, tc := range []struct {
		code   int
		reason string
		want   error
	}{
		{protocol.CloseSuspended, protocol.CloseReasonSuspended, reader.ErrSuspended},
		{protocol.CloseKeyRevoked, protocol.CloseReasonKeyRevoked, reader.ErrAdmitted},
		{protocol.CloseKicked, protocol.CloseReasonKicked, reader.ErrKicked},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			name := UniqueSheet(t)
			r := NewReader(t, srv, name)
			f := session.Filter{Sheet: name}
			Eventually(t, "session registered", func() bool { return len(srv.App.Sessions().List(f)) == 1 })
			if n := srv.App.Sessions().KillWhere(f, tc.code, tc.reason); n != 1 {
				t.Fatalf("KillWhere closed %d sessions, want 1", n)
			}
			deadline := time.Now().Add(time.Second)
			for r.LastError() != tc.want {
				if time.Now().After(deadline) {
					t.Fatalf("LastError = %v after 1s, want %v", r.LastError(), tc.want)
				}
				time.Sleep(5 * time.Millisecond)
			}
			if r.IsConnected() {
				t.Fatal("IsConnected after a terminal close")
			}
			Eventually(t, "session removed", func() bool { return len(srv.App.Sessions().List(f)) == 0 })
			time.Sleep(3 * reconnectMax)
			if n := len(srv.App.Sessions().List(f)); n != 0 {
				t.Fatalf("reader reopened after close %d: %d sessions", tc.code, n)
			}
			if err := r.Set(t.Context(), "k", 1); !errors.Is(err, reader.ErrClosed) {
				t.Fatalf("Set after terminal close = %v", err)
			}
		})
	}
}

// TestTerminalNonTerminalKickReconnects (R1 regression): a 1001 kick of every collaborator
// (step-down shape) is not terminal — the reader reconnects and LastError stays nil.
func TestTerminalNonTerminalKickReconnects(t *testing.T) {
	srv := StartLeader(t)
	name := UniqueSheet(t)
	r := NewReader(t, srv, name)
	f := session.Filter{Sheet: name}
	Eventually(t, "session registered", func() bool { return len(srv.App.Sessions().List(f)) == 1 })
	first := srv.App.Sessions().List(f)[0].ID
	srv.App.Registry().KickAll("test kick")
	Eventually(t, "reader back on a new session", func() bool {
		l := srv.App.Sessions().List(f)
		return len(l) == 1 && l[0].ID != first && r.IsConnected()
	})
	if r.LastError() != nil {
		t.Fatalf("LastError = %v after a 1001 kick", r.LastError())
	}
}

// TestTerminalOpenSuspended (D-027): an open of a suspended project is refused and closed 4001;
// reader.New returns ErrSuspended.
func TestTerminalOpenSuspended(t *testing.T) {
	srv := StartLeader(t)
	name := UniqueSheet(t)
	NewReader(t, srv, name).Close() // autocreates the sheet
	s, ok := srv.App.Registry().Get(name)
	if !ok {
		t.Fatal("sheet not created")
	}
	srv.App.Tenants().SetSuspended(s.ProjectID(), true)
	t.Cleanup(func() { srv.App.Tenants().SetSuspended(s.ProjectID(), false) })
	_, err := reader.New(reader.Config{
		LeaderAddr: srv.WSURL, Sheet: name, Password: "pw",
		ReconnectMin: 10 * time.Millisecond, ReconnectMax: 100 * time.Millisecond, ConnectTimeout: 5 * time.Second,
	})
	if !errors.Is(err, reader.ErrSuspended) {
		t.Fatalf("New on a suspended project = %v, want ErrSuspended", err)
	}
}
