// Covers: POST /admin/projects/{id}/suspend, POST /admin/projects/{id}/resume, tenancy control (stream + NOTIFY), suspended reconcile and boot load (criterion 6)
package integration

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/CrimsonCoder42/red_lite_cache/internal/config"
	"github.com/CrimsonCoder42/red_lite_cache/internal/session"
	"github.com/CrimsonCoder42/red_lite_cache/protocol"
)

// plantSession registers a session of project on srv's node (a replica refuses opens until it
// is promoted, so this is how a test gives it a session to close) and returns the close code
// channel its Kill reports to.
func plantSession(t *testing.T, srv *Server, project uuid.UUID, sheet string) <-chan int {
	t.Helper()
	ch := make(chan int, 1)
	s := session.New(session.Params{Project: project, Sheet: sheet, Remote: "test", Kill: func(code int, _ string) { ch <- code }}, time.Now())
	srv.App.Sessions().Add(s)
	t.Cleanup(func() { srv.App.Sessions().Remove(s.ID()) })
	return ch
}

func expectKill(t *testing.T, ch <-chan int, code int) {
	t.Helper()
	select {
	case got := <-ch:
		if got != code {
			t.Fatalf("planted session closed %d, want %d", got, code)
		}
	case <-time.After(time.Second):
		t.Fatalf("planted session not closed within 1s (want %d)", code)
	}
}

func setEntry(t *testing.T, c *RawConn, key, value string) {
	t.Helper()
	if r := c.Do(protocol.Request{Action: protocol.ActionSet, Key: key, Value: json.RawMessage(value), TTL: 10}); !r.Success {
		t.Fatalf("set %s: %+v", key, r)
	}
}

// TestSuspendLeaderAndReplica is criterion 6 with persistence off (leader → replica control
// frames): suspend closes the leader's session (4001) and the replica applies it (suspended, and a
// session it holds closes 4001); open is refused "suspended" + 4001; resume re-admits on both and
// the entries are intact on leader and replica.
func TestSuspendLeaderAndReplica(t *testing.T) {
	leader := StartLeader(t, tnKeyMode)
	rep := StartReplica(t, leader, tnKeyMode)
	p, name := tnProject(t, leader)
	pid := uuid.MustParse(p.ProjectID)
	c := tnMustOpen(t, leader, name, p.APIKey)
	setEntry(t, c, "kept", `{"v":1}`)
	Eventually(t, "entry on the replica", func() bool {
		s, ok := rep.App.Registry().Get(name)
		return ok && s.Len() == 1
	})
	planted := plantSession(t, rep, pid, name)
	other, otherName := tnProject(t, leader)
	bystander := tnMustOpen(t, leader, otherName, other.APIKey)

	if code := adminDo(t, leader, "POST", "/admin/projects/"+p.ProjectID+"/suspend", testAdminKey, nil, nil); code != http.StatusNoContent {
		t.Fatalf("suspend = %d", code)
	}
	tnClosedWith(t, c, protocol.CloseSuspended, time.Second)
	expectKill(t, planted, protocol.CloseSuspended)
	Eventually(t, "replica applied the suspend", func() bool { return rep.App.Tenants().Suspended(pid) })
	tnAlive(t, bystander) // another project is untouched

	refused, resp := tnOpen(t, leader, name, p.APIKey)
	if resp.Success || resp.Error != protocol.ErrMsgSuspended {
		t.Fatalf("open while suspended = %+v", resp)
	}
	tnClosedWith(t, refused, protocol.CloseSuspended, time.Second)
	var d protocol.ProjectDetail
	adminDo(t, leader, "GET", "/admin/projects/"+p.ProjectID, testAdminKey, nil, &d)
	if d.SuspendedAt == "" {
		t.Fatalf("detail while suspended = %+v", d.ProjectSummary)
	}

	if code := adminDo(t, leader, "POST", "/admin/projects/"+p.ProjectID+"/resume", testAdminKey, nil, nil); code != http.StatusNoContent {
		t.Fatalf("resume = %d", code)
	}
	Eventually(t, "replica applied the resume", func() bool { return !rep.App.Tenants().Suspended(pid) })
	back := tnMustOpen(t, leader, name, p.APIKey)
	if r := back.Do(protocol.Request{Action: protocol.ActionGet, Key: "kept"}); !r.Success || string(r.Value) != `{"v":1}` {
		t.Fatalf("entry after resume = %+v", r)
	}
	if s, ok := rep.App.Registry().Get(name); !ok || s.Len() != 1 {
		t.Fatal("replica lost the entry across suspend/resume")
	}

	if code := adminDo(t, leader, "POST", "/admin/projects/"+uuid.NewString()+"/suspend", testAdminKey, nil, nil); code != http.StatusNotFound {
		t.Fatalf("suspend of an unknown project = %d, want 404", code)
	}
}

// TestSuspendControlKinds: the replica applies a key_revoked and a session_kill control frame
// (persistence off) to the sessions it holds, and a revoke on the leader reaches it.
func TestSuspendControlKinds(t *testing.T) {
	leader := StartLeader(t, tnKeyMode)
	rep := StartReplica(t, leader, tnKeyMode)
	p, name := tnProject(t, leader)
	k := tnIssue(t, leader, p.ProjectID, "")
	pid, kid := uuid.MustParse(p.ProjectID), uuid.MustParse(k.KeyID)

	ch := make(chan int, 1)
	s := session.New(session.Params{Project: pid, Sheet: name, KeyID: kid, Kill: func(code int, _ string) { ch <- code }}, time.Now())
	rep.App.Sessions().Add(s)
	t.Cleanup(func() { rep.App.Sessions().Remove(s.ID()) })
	untouched := plantSession(t, rep, pid, name) // no key id: must survive the key revoke

	if code := adminDo(t, leader, "DELETE", "/admin/keys/"+k.KeyID, testAdminKey, nil, nil); code != http.StatusNoContent {
		t.Fatalf("revoke = %d", code)
	}
	expectKill(t, ch, protocol.CloseKeyRevoked)
	select {
	case code := <-untouched:
		t.Fatalf("a session on no key closed %d on a key revoke", code)
	case <-time.After(200 * time.Millisecond):
	}

	leader.App.Node().SendControl(protocol.ReplicaControl{Type: protocol.TypeSessionKill, Session: "not-a-uuid"}) // ignored
	leader.App.Node().SendControl(protocol.ReplicaControl{Type: protocol.TypeKeyRevoked})                         // no key id: ignored, never "every session"
	select {
	case code := <-untouched:
		t.Fatalf("a malformed control closed a session (%d)", code)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestSuspendPersisted (DB): with persistence a suspend reaches the replica, survives a leader
// restart (boot load of the suspended set), and a change made only in Postgres is picked up by the
// cleaner-tick reconcile (a missed NOTIFY).
func TestSuspendPersisted(t *testing.T) {
	dbURL := persistURL(t)
	tick := 300 * time.Millisecond
	mods := []func(*config.Config){withPersist(dbURL), tnKeyMode, func(c *config.Config) { c.PersistCleanupInterval = tick }}
	leader := StartLeader(t, mods...)
	rep := StartReplica(t, leader, mods...)
	p, name := tnProject(t, leader)
	pid := uuid.MustParse(p.ProjectID)
	c := tnMustOpen(t, leader, name, p.APIKey)
	setEntry(t, c, "kept", `2`)
	planted := plantSession(t, rep, pid, name)

	if code := adminDo(t, leader, "POST", "/admin/projects/"+p.ProjectID+"/suspend", testAdminKey, nil, nil); code != http.StatusNoContent {
		t.Fatalf("suspend = %d", code)
	}
	tnClosedWith(t, c, protocol.CloseSuspended, time.Second)
	expectKill(t, planted, protocol.CloseSuspended)
	Eventually(t, "replica suspended", func() bool { return rep.App.Tenants().Suspended(pid) })
	Eventually(t, "entry flushed", func() bool { return rowCount(t, leader, name) == 1 })

	// Resume only in Postgres: the reconcile lifts it on both instances within a tick or two.
	if _, err := leader.App.Store().Pool().Exec(t.Context(), "UPDATE projects SET suspended_at = NULL WHERE id = $1", pid); err != nil {
		t.Fatalf("update: %v", err)
	}
	Eventually(t, "reconcile resumed the leader", func() bool { return !leader.App.Tenants().Suspended(pid) })
	Eventually(t, "reconcile resumed the replica", func() bool { return !rep.App.Tenants().Suspended(pid) })

	// Suspend only in Postgres, then restart the leader: the boot load refuses the open at once.
	if _, err := leader.App.Store().Pool().Exec(t.Context(), "UPDATE projects SET suspended_at = now() WHERE id = $1", pid); err != nil {
		t.Fatalf("update: %v", err)
	}
	rep.Stop()
	leader.Stop()
	again := StartLeader(t, mods...)
	if !again.App.Tenants().Suspended(pid) {
		t.Fatal("restarted leader did not load the suspended set")
	}
	if _, resp := tnOpen(t, again, name, p.APIKey); resp.Error != protocol.ErrMsgSuspended {
		t.Fatalf("open after restart = %+v", resp)
	}
	if code := adminDo(t, again, "POST", "/admin/projects/"+p.ProjectID+"/resume", testAdminKey, nil, nil); code != http.StatusNoContent {
		t.Fatalf("resume = %d", code)
	}
	back := tnMustOpen(t, again, name, p.APIKey)
	if r := back.Do(protocol.Request{Action: protocol.ActionGet, Key: "kept"}); !r.Success || string(r.Value) != `2` {
		t.Fatalf("entry after restart + resume = %+v", r)
	}
}
