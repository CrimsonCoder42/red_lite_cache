// Covers: POST /admin/projects/{id}/revoke, control bus (D-021), advisory election (D-022)
package integration

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/CrimsonCoder42/red_lite_cache/internal/config"
	"github.com/CrimsonCoder42/red_lite_cache/protocol"
)

const testAdminKey = "test-admin-key"

func adminOn(c *config.Config) { c.AdminKey = testAdminKey; c.SheetAutocreate = false }

// adminDo calls the management API on srv with key as bearer and decodes the body into out.
func adminDo(t *testing.T, srv *Server, method, path, key string, body, out any) int {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		json.NewEncoder(&buf).Encode(body)
	}
	req, _ := http.NewRequestWithContext(t.Context(), method, srv.URL+path, &buf)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	if out != nil {
		json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

// projectWithSheet creates a project and one sheet (password "pw") through srv's API.
func projectWithSheet(t *testing.T, srv *Server) (protocol.CreateProjectResponse, string) {
	t.Helper()
	var p protocol.CreateProjectResponse
	if code := adminDo(t, srv, "POST", "/admin/projects", testAdminKey, protocol.CreateProjectRequest{Name: UniqueSheet(t)}, &p); code != http.StatusCreated {
		t.Fatalf("create project: %d", code)
	}
	name := UniqueSheet(t)
	if code := adminDo(t, srv, "POST", "/admin/projects/"+p.ProjectID+"/sheets", p.APIKey, protocol.CreateSheetRequest{Name: name, Password: "pw"}, nil); code != http.StatusCreated {
		t.Fatalf("create sheet: %d", code)
	}
	return p, name
}

// TestRevokeProjectMemStore: with persistence off the revoke works locally: the project's keys
// stop working, its sheet is dropped, its collaborators are kicked, and open answers sheet not
// found (autocreate off).
func TestRevokeProjectMemStore(t *testing.T) {
	srv := StartLeader(t, adminOn)
	p, name := projectWithSheet(t, srv)
	other, otherSheet := projectWithSheet(t, srv)
	joined, _ := OpenRaw(t, srv, name)

	if code := adminDo(t, srv, "POST", "/admin/projects/"+p.ProjectID+"/revoke", "", nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("revoke without admin key = %d", code)
	}
	if code := adminDo(t, srv, "POST", "/admin/projects/"+p.ProjectID+"/revoke", p.APIKey, nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("revoke with a project key = %d, want 401 (admin only)", code)
	}
	if code := adminDo(t, srv, "POST", "/admin/projects/00000000-0000-0000-0000-00000000abcd/revoke", testAdminKey, nil, nil); code != http.StatusNotFound {
		t.Fatalf("revoke unknown project = %d", code)
	}
	if code := adminDo(t, srv, "POST", "/admin/projects/"+p.ProjectID+"/revoke", testAdminKey, nil, nil); code != http.StatusNoContent {
		t.Fatalf("revoke = %d", code)
	}

	joined.ExpectClosed(Timeout)
	if _, ok := srv.App.Registry().Get(name); ok {
		t.Fatal("revoked project's sheet still in the registry")
	}
	if r, _ := Dial(t, srv.WSURL).Open(name, "pw"); r.Error != protocol.ErrMsgSheetNotFound {
		t.Fatalf("open after revoke = %+v", r)
	}
	if code := adminDo(t, srv, "POST", "/admin/projects/"+p.ProjectID+"/sheets", p.APIKey, protocol.CreateSheetRequest{Name: "again", Password: "pw"}, nil); code != http.StatusUnauthorized {
		t.Fatalf("revoked key still works: %d", code)
	}
	// Another project is untouched.
	if r, _ := Dial(t, srv.WSURL).Open(otherSheet, "pw"); !r.Success {
		t.Fatalf("other project's sheet after revoke = %+v (project %s)", r, other.ProjectID)
	}
}

// TestRevokeReplicaRefusesAdmin: the revoke route is leader-only.
func TestRevokeReplicaRefusesAdmin(t *testing.T) {
	leader := StartLeader(t, adminOn)
	rep := StartReplica(t, leader, adminOn)
	if code := adminDo(t, rep, "POST", "/admin/projects/00000000-0000-0000-0000-00000000abcd/revoke", testAdminKey, nil, nil); code != http.StatusServiceUnavailable {
		t.Fatalf("revoke on a replica = %d, want 503", code)
	}
}

// revokeAcrossInstances: leader + replica on one schema; a revoke on the leader removes the sheet
// from both memories within one cleaner tick (listen=true: by NOTIFY; listen=false: by the
// cleaner-tick reconcile alone) and open refuses on every instance.
func revokeAcrossInstances(t *testing.T, listen bool) {
	dbURL := persistURL(t)
	tick := 300 * time.Millisecond
	mods := []func(*config.Config){withPersist(dbURL), adminOn, func(c *config.Config) {
		c.PersistCleanupInterval = tick
		c.ControlListen = listen
	}}
	leader := StartLeader(t, mods...)
	rep := StartReplica(t, leader, mods...)
	_, name := projectWithSheet(t, leader)
	Eventually(t, "sheet on the replica and in Postgres", func() bool {
		_, ok := rep.App.Registry().Get(name)
		var n int
		leader.App.Store().Pool().QueryRow(t.Context(), "SELECT count(*) FROM sheets WHERE name = $1", name).Scan(&n)
		return ok && n == 1
	})
	time.Sleep(2 * tick) // the reconcile has seen the row at least once

	if code := adminDo(t, leader, "POST", "/admin/projects/"+projectOf(t, leader, name)+"/revoke", testAdminKey, nil, nil); code != http.StatusNoContent {
		t.Fatalf("revoke = %d", code)
	}
	revoked := time.Now()
	Eventually(t, "replica dropped the sheet", func() bool {
		_, ok := rep.App.Registry().Get(name)
		return !ok
	})
	if took := time.Since(revoked); took > tick+time.Second {
		t.Fatalf("replica dropped the sheet after %s, want within one tick (%s)", took, tick)
	}
	for _, s := range []*Server{leader, rep} {
		r, _ := Dial(t, s.WSURL).Open(name, "pw")
		if r.Success {
			t.Fatalf("open on %s after revoke succeeded", s.Cfg.InstanceID)
		}
	}
	if r, _ := Dial(t, leader.WSURL).Open(name, "pw"); r.Error != protocol.ErrMsgSheetNotFound {
		t.Fatalf("open on the leader after revoke = %+v", r)
	}
}

func projectOf(t *testing.T, srv *Server, sheetName string) string {
	t.Helper()
	s, ok := srv.App.Registry().Get(sheetName)
	if !ok {
		t.Fatalf("no sheet %s", sheetName)
	}
	return s.ProjectID().String()
}

func TestRevokeEveryInstanceByNotify(t *testing.T)    { revokeAcrossInstances(t, true) }
func TestRevokeEveryInstanceByReconcile(t *testing.T) { revokeAcrossInstances(t, false) }

// TestAdvisoryElection (D-022): two instances on one database; the lock holder leads, the other
// follows and cannot promote while the lock is held; stop the holder and the other acquires the
// lock and promotes with a higher epoch.
func TestAdvisoryElection(t *testing.T) {
	dbURL := persistURL(t)
	advisory := func(c *config.Config) { c.Election = "advisory" }
	a := StartLeader(t, withPersist(dbURL), advisory)
	Eventually(t, "A holds the lock and leads", a.App.Node().IsLeader)
	ea := a.App.Node().Epoch()
	if ea < 1 {
		t.Fatalf("A epoch = %d", ea)
	}
	b := StartReplica(t, a, withPersist(dbURL), advisory)

	// A third node told it is a leader cannot lead while A holds the lock.
	c := StartLeader(t, withPersist(dbURL), advisory, func(cfg *config.Config) { cfg.InstanceID += "-C"; cfg.PeerAddr = a.WSURL })
	time.Sleep(3 * c.Cfg.HeartbeatTimeout)
	if c.App.Node().IsLeader() || b.App.Node().IsLeader() {
		t.Fatalf("a second leader without the lock: b=%v c=%v", b.App.Node().IsLeader(), c.App.Node().IsLeader())
	}
	c.Stop()

	a.Stop()
	Eventually(t, "B acquires the lock and promotes", b.App.Node().IsLeader)
	if eb := b.App.Node().Epoch(); eb <= ea {
		t.Fatalf("B epoch %d not above A's %d", eb, ea)
	}
}
