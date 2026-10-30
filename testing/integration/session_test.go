// Covers: sessions on /ws (D-026: create on open, last_seen/frames, remove on close) and kill by close code (D-030, D-028)
package integration

import (
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/CrimsonCoder42/red_lite_cache/internal/session"
	"github.com/CrimsonCoder42/red_lite_cache/protocol"
)

// expectClose fails unless the next read on c ends with close status code within d.
func expectClose(t *testing.T, c *RawConn, code websocket.StatusCode, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		_, err := c.TryNext(time.Until(deadline))
		if err == nil {
			continue // a frame queued before the close
		}
		if st := websocket.CloseStatus(err); st != code {
			t.Fatalf("read err %v status %d, want close %d within %s", err, st, code, d)
		}
		return
	}
}

// TestSessionLoop is the SESSION loop: an open with a key is listed with its fields, inbound
// frames advance last_seen and frames_in, KillWhere closes the socket with 4003 within 1 s and the
// session leaves the registry; a kill by key id closes only that key's session (4002).
func TestSessionLoop(t *testing.T) {
	srv := StartLeader(t, production)
	a := newProject(t, srv)
	name := newSheet(t, srv, a.Project)
	sessions := srv.App.Sessions()

	c := Dial(t, srv.WSURL)
	open := c.openKey(name, "pw", a.Key)
	if !open.Success {
		t.Fatalf("open = %+v", open)
	}
	list := sessions.List(session.Filter{Project: a.Project})
	if len(list) != 1 {
		t.Fatalf("sessions of the project = %d, want 1", len(list))
	}
	s0 := list[0]
	if s0.Project != a.Project || s0.Sheet != name || s0.KeyID != a.KeyID || s0.Remote == "" ||
		s0.ConnectedAt.IsZero() || s0.Epoch != open.Epoch || s0.Node != srv.App.InstanceID() {
		t.Fatalf("session = %+v, want project %s sheet %s key %s epoch %d node %s", s0, a.Project, name, a.KeyID, open.Epoch, srv.App.InstanceID())
	}
	// The writer counts a frame after its write returns, which can be after the peer read it.
	project := func() session.Info { return sessions.List(session.Filter{Project: a.Project})[0] }
	Eventually(t, "frames_out counts the open reply and sheet_state", func() bool { return project().FramesOut >= 2 })

	time.Sleep(20 * time.Millisecond)
	for range 3 {
		if r := c.Do(protocol.Request{Action: protocol.ActionGet, Key: "missing"}); r.Error != protocol.ErrMsgKeyNotFound {
			t.Fatalf("get = %+v", r)
		}
	}
	info := project()
	if !info.LastSeen.After(s0.LastSeen) || info.FramesIn < s0.FramesIn+3 {
		t.Fatalf("after 3 gets: last_seen %v → %v, frames_in %d → %d", s0.LastSeen, info.LastSeen, s0.FramesIn, info.FramesIn)
	}
	Eventually(t, "frames_out counts the 3 replies", func() bool { return project().FramesOut >= 5 })

	if n := sessions.KillWhere(session.Filter{Project: a.Project}, protocol.CloseKicked, protocol.CloseReasonKicked); n != 1 {
		t.Fatalf("KillWhere killed %d, want 1", n)
	}
	expectClose(t, c, protocol.CloseKicked, time.Second)
	Eventually(t, "killed session removed", func() bool { _, ok := sessions.Get(s0.ID); return !ok })

	t.Run("kill by key closes only that key's session", func(t *testing.T) {
		k2 := issueKey(t, srv, a.Project)
		c1, c2 := Dial(t, srv.WSURL), Dial(t, srv.WSURL)
		if r := c1.openKey(name, "pw", a.Key); !r.Success {
			t.Fatalf("open k1 = %+v", r)
		}
		if r := c2.openKey(name, "pw", k2.Key); !r.Success {
			t.Fatalf("open k2 = %+v", r)
		}
		if n := sessions.KillWhere(session.Filter{KeyID: a.KeyID}, protocol.CloseKeyRevoked, protocol.CloseReasonKeyRevoked); n != 1 {
			t.Fatalf("KillWhere(key 1) killed %d, want 1", n)
		}
		expectClose(t, c1, protocol.CloseKeyRevoked, time.Second)
		if r := c2.Do(protocol.Request{Action: protocol.ActionGet, Key: "missing"}); r.Error != protocol.ErrMsgKeyNotFound {
			t.Fatalf("key 2 session after key 1's kill: get = %+v", r)
		}
		Eventually(t, "only key 2's session left", func() bool {
			l := sessions.List(session.Filter{Project: a.Project})
			return len(l) == 1 && l[0].KeyID == k2.KeyID
		})
	})
}
