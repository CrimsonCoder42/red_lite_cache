// Covers: internal/api tenancy routes — GET /admin/projects[/{id}], POST /admin/projects/{id}/keys, DELETE /admin/keys/{id}, POST /admin/keys/{id}/rotate, GET /admin/sessions, GET /admin/sheets/{name}/sessions, DELETE /admin/sessions/{id}, audit (criteria 3, 4, 5-route, 7, 8, 11)
package integration

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/CrimsonCoder42/red_lite_cache/internal/config"
	"github.com/CrimsonCoder42/red_lite_cache/protocol"
	"github.com/CrimsonCoder42/red_lite_cache/reader"
)

const tnClusterToken = "tn-cluster-token"

// tnKeyMode is production-shaped admission: key mode, no autocreate, admin key, cluster token.
func tnKeyMode(c *config.Config) {
	c.AdminKey = testAdminKey
	c.SheetAutocreate = false
	c.Admission = "key"
	c.Environment = "production"
	c.ClusterToken = tnClusterToken
}

// tnProject creates a project and one sheet (password "pw") on srv and returns them.
func tnProject(t *testing.T, srv *Server) (protocol.CreateProjectResponse, string) {
	t.Helper()
	return projectWithSheet(t, srv) // control_test.go: POST /admin/projects + POST sheets with the project key
}

// tnOpen dials srv and opens sheet with password "pw" and key; on success it consumes sheet_state.
func tnOpen(t *testing.T, srv *Server, sheet, key string) (*RawConn, protocol.Response) {
	t.Helper()
	c := Dial(t, srv.WSURL)
	resp := c.Do(protocol.Request{Action: protocol.ActionOpen, Sheet: sheet, Password: "pw", APIKey: key})
	if resp.Success {
		var st protocol.SheetState
		c.NextType(protocol.TypeSheetState, &st)
	}
	return c, resp
}

// tnMustOpen is tnOpen that fails the test on a refusal.
func tnMustOpen(t *testing.T, srv *Server, sheet, key string) *RawConn {
	t.Helper()
	c, resp := tnOpen(t, srv, sheet, key)
	if !resp.Success {
		t.Fatalf("open %s: %+v", sheet, resp)
	}
	return c
}

// tnClosedWith fails unless c's socket is closed by the server with code within d.
func tnClosedWith(t *testing.T, c *RawConn, code int, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		left := time.Until(deadline)
		if left <= 0 {
			t.Fatalf("socket still open after %s (want close %d)", d, code)
		}
		_, err := c.TryNext(left)
		if err == nil {
			continue // frames sent before the close
		}
		if errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("socket still open after %s (want close %d)", d, code)
		}
		if got := int(websocket.CloseStatus(err)); got != code {
			t.Fatalf("close code = %d (%v), want %d", got, err, code)
		}
		return
	}
}

// tnAlive fails unless c still answers a get.
func tnAlive(t *testing.T, c *RawConn) {
	t.Helper()
	if r := c.Do(protocol.Request{Action: protocol.ActionGet, Key: "nope"}); r.Error != protocol.ErrMsgKeyNotFound && !r.Success {
		t.Fatalf("session not alive: %+v", r)
	}
}

func tnSessions(t *testing.T, srv *Server, path string) protocol.Page[protocol.SessionInfo] {
	t.Helper()
	var page protocol.Page[protocol.SessionInfo]
	if code := adminDo(t, srv, "GET", path, testAdminKey, nil, &page); code != http.StatusOK {
		t.Fatalf("GET %s = %d", path, code)
	}
	return page
}

func tnFind(page protocol.Page[protocol.SessionInfo], sheet string) (protocol.SessionInfo, bool) {
	for _, s := range page.Items {
		if s.Sheet == sheet {
			return s, true
		}
	}
	return protocol.SessionInfo{}, false
}

func tnDetail(t *testing.T, srv *Server, project string) protocol.ProjectDetail {
	t.Helper()
	var d protocol.ProjectDetail
	if code := adminDo(t, srv, "GET", "/admin/projects/"+project, testAdminKey, nil, &d); code != http.StatusOK {
		t.Fatalf("GET project = %d", code)
	}
	return d
}

func tnIssue(t *testing.T, srv *Server, project, label string) protocol.IssuedKey {
	t.Helper()
	var k protocol.IssuedKey
	if code := adminDo(t, srv, "POST", "/admin/projects/"+project+"/keys", testAdminKey, protocol.IssueKeyRequest{Label: label}, &k); code != http.StatusCreated {
		t.Fatalf("issue key = %d", code)
	}
	return k
}

// TestTenancyRegisterToOpen is criterion 3: admin creates a project, the project key creates a
// sheet, a reader with Config.APIKey opens it in production (key) mode and writes; without the
// key the same reader cannot open.
func TestTenancyRegisterToOpen(t *testing.T) {
	srv := StartLeader(t, tnKeyMode)
	p, name := tnProject(t, srv)
	r := NewReader(t, srv, name, func(c *reader.Config) { c.APIKey = p.APIKey })
	if !r.IsConnected() {
		t.Fatal("reader with the project key did not open")
	}
	if err := r.SetWithTTL(context.Background(), "k", 1, 5); err != nil {
		t.Fatalf("set: %v", err)
	}
	if _, resp := tnOpen(t, srv, name, ""); resp.Success || resp.Error != protocol.ErrMsgNotAuthorized {
		t.Fatalf("open without a key = %+v, want not authorized", resp)
	}
}

// TestTenancySessionListing is criterion 4: the session is listed with project, sheet, key,
// remote, connected_at, an advancing last_seen and this node's id; the sheet route and the project
// detail see it too; the key's last_used_at is recorded.
func TestTenancySessionListing(t *testing.T) {
	srv := StartLeader(t, tnKeyMode)
	p, name := tnProject(t, srv)
	c := tnMustOpen(t, srv, name, p.APIKey)

	first, ok := tnFind(tnSessions(t, srv, "/admin/sessions"), name)
	if !ok {
		t.Fatal("session not listed")
	}
	if first.ProjectID != p.ProjectID || first.Remote == "" || first.ConnectedAt == "" || first.KeyID == "" ||
		first.Node != srv.App.InstanceID() || first.SessionID == "" {
		t.Fatalf("listing row = %+v", first)
	}
	time.Sleep(20 * time.Millisecond)
	tnAlive(t, c)
	second, _ := tnFind(tnSessions(t, srv, "/admin/sessions?project="+p.ProjectID), name)
	t1, _ := time.Parse(time.RFC3339Nano, first.LastSeen)
	t2, _ := time.Parse(time.RFC3339Nano, second.LastSeen)
	if !t2.After(t1) || second.FramesIn <= first.FramesIn {
		t.Fatalf("last_seen did not advance: %s → %s (frames_in %d → %d)", first.LastSeen, second.LastSeen, first.FramesIn, second.FramesIn)
	}
	if page := tnSessions(t, srv, "/admin/sheets/"+name+"/sessions"); len(page.Items) != 1 || page.Items[0].SessionID != first.SessionID {
		t.Fatalf("sheet sessions = %+v", page)
	}
	d := tnDetail(t, srv, p.ProjectID)
	if d.Keys != 1 || d.Sheets != 1 || len(d.SheetsList) != 1 || d.SheetsList[0].Collaborators != 1 || len(d.APIKeys) != 1 ||
		d.APIKeys[0].KeyPrefix != p.KeyPrefix || d.Name == "" || d.CreatedAt == "" {
		t.Fatalf("project detail = %+v", d)
	}
	Eventually(t, "key last_used_at recorded", func() bool { return tnDetail(t, srv, p.ProjectID).APIKeys[0].LastUsedAt != "" })
}

// TestTenancyKillSession is criterion 5's route half: DELETE /admin/sessions/{id} closes the raw
// conn with 4003 within 1 s; an unknown id is 404.
func TestTenancyKillSession(t *testing.T) {
	srv := StartLeader(t, tnKeyMode)
	p, name := tnProject(t, srv)
	c := tnMustOpen(t, srv, name, p.APIKey)
	s, _ := tnFind(tnSessions(t, srv, "/admin/sessions"), name)
	if code := adminDo(t, srv, "DELETE", "/admin/sessions/"+s.SessionID, testAdminKey, nil, nil); code != http.StatusNoContent {
		t.Fatalf("kill = %d", code)
	}
	tnClosedWith(t, c, protocol.CloseKicked, time.Second)
	Eventually(t, "session gone from the listing", func() bool {
		_, ok := tnFind(tnSessions(t, srv, "/admin/sessions"), name)
		return !ok
	})
	if code := adminDo(t, srv, "DELETE", "/admin/sessions/"+s.SessionID, testAdminKey, nil, nil); code != http.StatusNotFound {
		t.Fatalf("second kill = %d, want 404", code)
	}
	if code := adminDo(t, srv, "DELETE", "/admin/sessions/not-a-uuid", testAdminKey, nil, nil); code != http.StatusBadRequest {
		t.Fatalf("bad id = %d, want 400", code)
	}
}

// TestTenancyPeerSessions is criterion 4's node tag across a pair: the replica's GET /admin/sessions
// merges the leader's sessions (fetched with ?scope=local), tagged with the leader's node; the
// replica's own scope=local listing is empty (a replica refuses opens until it is promoted, so the
// leader is the node that holds sessions). DELETE on the replica forwards the kill to the leader.
func TestTenancyPeerSessions(t *testing.T) {
	leader := StartLeader(t, tnKeyMode)
	rep := StartReplica(t, leader, tnKeyMode)
	p, name := tnProject(t, leader)
	c := tnMustOpen(t, leader, name, p.APIKey)

	s, ok := tnFind(tnSessions(t, rep, "/admin/sessions"), name)
	if !ok || s.Node != leader.App.InstanceID() {
		t.Fatalf("replica listing row = %+v (found %v), want node %s", s, ok, leader.App.InstanceID())
	}
	if page := tnSessions(t, rep, "/admin/sessions?scope=local"); len(page.Items) != 0 {
		t.Fatalf("replica local sessions = %+v", page.Items)
	}
	if page := tnSessions(t, rep, "/admin/sheets/"+name+"/sessions"); len(page.Items) != 1 {
		t.Fatalf("replica sheet sessions = %+v", page.Items)
	}
	if code := adminDo(t, rep, "DELETE", "/admin/sessions/"+s.SessionID, testAdminKey, nil, nil); code != http.StatusNoContent {
		t.Fatalf("forwarded kill = %d", code)
	}
	tnClosedWith(t, c, protocol.CloseKicked, time.Second)
}

// TestTenancyRevokeOneKey is criterion 7: two keys of one project, revoke one; only its session
// closes (4002), the other stays; the revoked key no longer admits.
func TestTenancyRevokeOneKey(t *testing.T) {
	srv := StartLeader(t, tnKeyMode)
	p, name := tnProject(t, srv)
	k2 := tnIssue(t, srv, p.ProjectID, "second")
	if k2.APIKey == "" || k2.Label != "second" || k2.ProjectID != p.ProjectID {
		t.Fatalf("issued = %+v", k2)
	}
	c1 := tnMustOpen(t, srv, name, p.APIKey)
	c2 := tnMustOpen(t, srv, name, k2.APIKey)

	d := tnDetail(t, srv, p.ProjectID)
	var k1ID string
	for _, k := range d.APIKeys {
		if k.KeyPrefix == p.KeyPrefix {
			k1ID = k.KeyID
		}
	}
	if d.Keys != 2 || k1ID == "" {
		t.Fatalf("detail before revoke = %+v", d)
	}
	if code := adminDo(t, srv, "DELETE", "/admin/keys/"+k1ID, testAdminKey, nil, nil); code != http.StatusNoContent {
		t.Fatalf("revoke = %d", code)
	}
	tnClosedWith(t, c1, protocol.CloseKeyRevoked, time.Second)
	tnAlive(t, c2)
	if _, resp := tnOpen(t, srv, name, p.APIKey); resp.Success || resp.Error != protocol.ErrMsgNotAuthorized {
		t.Fatalf("open with the revoked key = %+v", resp)
	}
	tnMustOpen(t, srv, name, k2.APIKey)
	if d := tnDetail(t, srv, p.ProjectID); d.Keys != 1 {
		t.Fatalf("live keys after revoke = %d", d.Keys)
	}
	if code := adminDo(t, srv, "DELETE", "/admin/keys/"+k1ID, testAdminKey, nil, nil); code != http.StatusNotFound {
		t.Fatalf("second revoke = %d, want 404", code)
	}
}

// TestTenancyRotateKey is criterion 8: rotate returns a new key once (a second rotate of the old
// id is 404), the old key's session closes 4002 and the old key is refused; the new key admits and
// keeps the old label.
func TestTenancyRotateKey(t *testing.T) {
	srv := StartLeader(t, tnKeyMode)
	p, name := tnProject(t, srv)
	k := tnIssue(t, srv, p.ProjectID, "api-3")
	c := tnMustOpen(t, srv, name, k.APIKey)

	var next protocol.IssuedKey
	if code := adminDo(t, srv, "POST", "/admin/keys/"+k.KeyID+"/rotate", testAdminKey, nil, &next); code != http.StatusCreated {
		t.Fatalf("rotate = %d", code)
	}
	if next.APIKey == "" || next.APIKey == k.APIKey || next.KeyID == k.KeyID || next.Label != "api-3" || next.ProjectID != p.ProjectID {
		t.Fatalf("rotated = %+v", next)
	}
	tnClosedWith(t, c, protocol.CloseKeyRevoked, time.Second)
	if _, resp := tnOpen(t, srv, name, k.APIKey); resp.Success {
		t.Fatalf("old key admitted after rotate: %+v", resp)
	}
	tnMustOpen(t, srv, name, next.APIKey)
	if code := adminDo(t, srv, "POST", "/admin/keys/"+k.KeyID+"/rotate", testAdminKey, nil, nil); code != http.StatusNotFound {
		t.Fatalf("second rotate of the old key = %d, want 404", code)
	}
}

// TestTenancyPaging: limit/cursor over projects and sessions; bad limit or cursor is 400; a limit
// above 200 is clamped.
func TestTenancyPaging(t *testing.T) {
	srv := StartLeader(t, tnKeyMode)
	want := map[string]bool{}
	var last protocol.CreateProjectResponse
	var lastName string
	for range 5 {
		last, lastName = tnProject(t, srv)
		want[last.ProjectID] = true
	}
	got := map[string]bool{}
	cursor, pages := "", 0
	for {
		var page protocol.Page[protocol.ProjectSummary]
		path := "/admin/projects?limit=2"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		if code := adminDo(t, srv, "GET", path, testAdminKey, nil, &page); code != http.StatusOK {
			t.Fatalf("GET %s = %d", path, code)
		}
		pages++
		if len(page.Items) > 2 {
			t.Fatalf("page of %d with limit 2", len(page.Items))
		}
		for _, it := range page.Items {
			if got[it.ProjectID] {
				t.Fatalf("project %s on two pages", it.ProjectID)
			}
			got[it.ProjectID] = true
			if it.Keys != 1 || it.Sheets != 1 {
				t.Fatalf("summary = %+v", it)
			}
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	for id := range want {
		if !got[id] {
			t.Fatalf("project %s never paged", id)
		}
	}
	if len(got) != 5 || pages != 3 {
		t.Fatalf("paged %d projects in %d pages, want 5 in 3", len(got), pages)
	}
	for _, bad := range []string{"limit=0", "limit=x", "cursor=%25%25", "cursor=bm90LWEtY3Vyc29y"} {
		if code := adminDo(t, srv, "GET", "/admin/projects?"+bad, testAdminKey, nil, nil); code != http.StatusBadRequest {
			t.Fatalf("GET /admin/projects?%s = %d, want 400", bad, code)
		}
	}
	var big protocol.Page[protocol.ProjectSummary]
	if code := adminDo(t, srv, "GET", "/admin/projects?limit=1000", testAdminKey, nil, &big); code != http.StatusOK || len(big.Items) != 5 {
		t.Fatalf("limit=1000 = %d, %d items", code, len(big.Items))
	}

	for range 3 {
		tnMustOpen(t, srv, lastName, last.APIKey)
	}
	p1 := tnSessions(t, srv, "/admin/sessions?project="+last.ProjectID+"&limit=2")
	if len(p1.Items) != 2 || p1.NextCursor == "" {
		t.Fatalf("session page 1 = %+v", p1)
	}
	p2 := tnSessions(t, srv, "/admin/sessions?project="+last.ProjectID+"&limit=2&cursor="+p1.NextCursor)
	if len(p2.Items) != 1 || p2.NextCursor != "" || p2.Items[0].SessionID == p1.Items[0].SessionID || p2.Items[0].SessionID == p1.Items[1].SessionID {
		t.Fatalf("session page 2 = %+v", p2)
	}
}

// TestTenancyAuth: every tenancy route refuses no key and a wrong key (401), and a project key on
// an admin-only route (401).
func TestTenancyAuth(t *testing.T) {
	srv := StartLeader(t, tnKeyMode)
	p, name := tnProject(t, srv)
	k := tnIssue(t, srv, p.ProjectID, "")
	routes := [][2]string{
		{"GET", "/admin/projects"}, {"GET", "/admin/projects/" + p.ProjectID},
		{"POST", "/admin/projects/" + p.ProjectID + "/keys"}, {"DELETE", "/admin/keys/" + k.KeyID},
		{"POST", "/admin/keys/" + k.KeyID + "/rotate"}, {"POST", "/admin/projects/" + p.ProjectID + "/suspend"},
		{"POST", "/admin/projects/" + p.ProjectID + "/resume"}, {"GET", "/admin/sessions"},
		{"GET", "/admin/sheets/" + name + "/sessions"}, {"DELETE", "/admin/sessions/" + k.KeyID},
	}
	for _, rt := range routes {
		for _, key := range []string{"", "wrong-key", p.APIKey} {
			if code := adminDo(t, srv, rt[0], rt[1], key, nil, nil); code != http.StatusUnauthorized {
				t.Fatalf("%s %s with key %q = %d, want 401", rt[0], rt[1], key[:min(len(key), 4)], code)
			}
		}
	}
	tnMustOpen(t, srv, name, k.APIKey) // nothing above changed state
}

// TestTenancyReplicaWrites: on a replica every tenancy write answers 503 not leader; listings and
// the session kill answer.
func TestTenancyReplicaWrites(t *testing.T) {
	leader := StartLeader(t, tnKeyMode)
	rep := StartReplica(t, leader, tnKeyMode)
	p, _ := tnProject(t, leader)
	k := tnIssue(t, leader, p.ProjectID, "")
	for _, rt := range [][2]string{
		{"POST", "/admin/projects/" + p.ProjectID + "/keys"}, {"DELETE", "/admin/keys/" + k.KeyID},
		{"POST", "/admin/keys/" + k.KeyID + "/rotate"}, {"POST", "/admin/projects/" + p.ProjectID + "/suspend"},
		{"POST", "/admin/projects/" + p.ProjectID + "/resume"},
	} {
		var e protocol.APIError
		if code := adminDo(t, rep, rt[0], rt[1], testAdminKey, nil, &e); code != http.StatusServiceUnavailable || e.Error != protocol.ErrMsgNotLeader {
			t.Fatalf("replica %s %s = %d %+v, want 503 not leader", rt[0], rt[1], code, e)
		}
	}
	if code := adminDo(t, rep, "GET", "/admin/projects", testAdminKey, nil, nil); code != http.StatusOK {
		t.Fatalf("replica GET /admin/projects = %d", code)
	}
	if code := adminDo(t, rep, "DELETE", "/admin/sessions/"+k.KeyID, testAdminKey, nil, nil); code != http.StatusNotFound {
		t.Fatalf("replica kill of an unknown session = %d, want 404", code)
	}
}

// TestTenancyAuditRows is criterion 11 for the admin routes (DB): each mutation writes one
// audit_events row with node = the instance id and no key material in detail.
func TestTenancyAuditRows(t *testing.T) {
	dbURL := persistURL(t)
	srv := StartLeader(t, withPersist(dbURL), tnKeyMode)
	p, name := tnProject(t, srv)
	k := tnIssue(t, srv, p.ProjectID, "audit")
	var next protocol.IssuedKey
	adminDo(t, srv, "POST", "/admin/keys/"+k.KeyID+"/rotate", testAdminKey, nil, &next)
	adminDo(t, srv, "DELETE", "/admin/keys/"+next.KeyID, testAdminKey, nil, nil)
	c := tnMustOpen(t, srv, name, p.APIKey)
	s, _ := tnFind(tnSessions(t, srv, "/admin/sessions"), name)
	adminDo(t, srv, "DELETE", "/admin/sessions/"+s.SessionID, testAdminKey, nil, nil)
	tnClosedWith(t, c, protocol.CloseKicked, time.Second)
	adminDo(t, srv, "POST", "/admin/projects/"+p.ProjectID+"/suspend", testAdminKey, nil, nil)
	adminDo(t, srv, "POST", "/admin/projects/"+p.ProjectID+"/resume", testAdminKey, nil, nil)

	want := []string{"project_created", "sheet_created", "key_issued", "key_rotated", "key_revoked", "session_kick", "project_suspended", "project_resumed"}
	secrets := []string{p.APIKey, k.APIKey, next.APIKey, "password", "hash"} // key_prefix (rlk_ + 4) is allowed
	Eventually(t, "every admin mutation audited", func() bool {
		rows, err := srv.App.Store().Pool().Query(t.Context(),
			"SELECT action, node, detail::text FROM audit_events WHERE project_id = $1", p.ProjectID)
		if err != nil {
			t.Fatalf("query audit: %v", err)
		}
		defer rows.Close()
		count := map[string]int{}
		for rows.Next() {
			var action, node, detail string
			if err := rows.Scan(&action, &node, &detail); err != nil {
				t.Fatalf("scan: %v", err)
			}
			if node != srv.App.InstanceID() {
				t.Fatalf("audit %s node = %q", action, node)
			}
			for _, sec := range secrets {
				if strings.Contains(detail, sec) {
					t.Fatalf("audit %s detail carries secret material: %s", action, detail)
				}
			}
			count[action]++
		}
		for _, a := range want {
			if count[a] != 1 {
				return false
			}
		}
		return true
	})
	var parts int
	srv.App.Store().Pool().QueryRow(t.Context(),
		"SELECT count(*) FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid WHERE c.relname LIKE 'audit_events_2%'").Scan(&parts)
	if parts < 2 {
		t.Fatalf("monthly audit partitions = %d, want this and next month", parts)
	}
}
