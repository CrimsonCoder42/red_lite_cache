// Covers: open admission (D-024 key/password mode, D-025 autocreate, D-027 suspended), tenancy criteria 1, 2, 12
package integration

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/CrimsonCoder42/red_lite_cache/internal/api"
	"github.com/CrimsonCoder42/red_lite_cache/internal/config"
	"github.com/CrimsonCoder42/red_lite_cache/internal/tenant"
	"github.com/CrimsonCoder42/red_lite_cache/protocol"
)

// production is the StartLeader mod for a production-mode leader: key admission, autocreate off,
// a cluster token and an admin key, set explicitly (derived defaults are computed only at Load).
func production(cfg *config.Config) {
	cfg.Environment = "production"
	cfg.Admission = "key"
	cfg.SheetAutocreate = false
	cfg.ClusterToken = "t"
	cfg.AdminKey = "admin-test-key"
}

// tenantKey is one project with one API key, created through the app's project store.
type tenantKey struct {
	Project uuid.UUID
	KeyID   uuid.UUID
	Key     string
}

// newProject registers a project and its first key on srv.
func newProject(t *testing.T, srv *Server) tenantKey {
	t.Helper()
	tk := tenantKey{Project: uuid.New(), KeyID: uuid.New(), Key: "rlk_" + uuid.NewString()}
	k := api.Key{ID: tk.KeyID, ProjectID: tk.Project, Hash: tenant.HashKey(tk.Key), Prefix: tk.Key[:8]}
	if err := srv.App.Projects().CreateProject(context.Background(), tk.Project, "p-"+tk.Project.String(), k); err != nil {
		t.Fatalf("create project: %v", err)
	}
	return tk
}

// issueKey adds another key to tk's project.
func issueKey(t *testing.T, srv *Server, project uuid.UUID) tenantKey {
	t.Helper()
	tk := tenantKey{Project: project, KeyID: uuid.New(), Key: "rlk_" + uuid.NewString()}
	k := api.Key{ID: tk.KeyID, ProjectID: project, Hash: tenant.HashKey(tk.Key), Prefix: tk.Key[:8]}
	if err := srv.App.Projects().IssueKey(context.Background(), k); err != nil {
		t.Fatalf("issue key: %v", err)
	}
	return tk
}

// newSheet registers a sheet with password "pw" for project.
func newSheet(t *testing.T, srv *Server, project uuid.UUID) string {
	t.Helper()
	name := UniqueSheet(t)
	if _, err := srv.App.Registry().Create(project, name, "pw"); err != nil {
		t.Fatalf("create sheet: %v", err)
	}
	return name
}

// openKey sends a key-mode open and, on success, reads the sheet_state that follows.
func (r *RawConn) openKey(sheet, password, key string) protocol.Response {
	r.t.Helper()
	resp := r.Do(protocol.Request{Action: protocol.ActionOpen, ID: 1, Sheet: sheet, Password: password, APIKey: key})
	if resp.Success {
		var st protocol.SheetState
		if err := json.Unmarshal(r.Next(), &st); err != nil || st.Type != protocol.TypeSheetState {
			r.t.Fatalf("expected sheet_state after open, got %+v (err %v)", st, err)
		}
	}
	return resp
}

// expectRefused fails unless the open was answered msg and the socket then closed with code.
func expectRefused(t *testing.T, c *RawConn, resp protocol.Response, msg string, code websocket.StatusCode) {
	t.Helper()
	if resp.Success || resp.Error != msg || resp.ID != 1 {
		t.Fatalf("open reply = %+v, want error %q echoing id 1", resp, msg)
	}
	_, err := c.TryNext(5 * time.Second)
	if st := websocket.CloseStatus(err); st != code {
		t.Fatalf("after %q: err %v status %d, want close %d", msg, err, st, code)
	}
}

// TestAdmissionLoop is the ADMISSION loop in production mode (criterion 1): no key, another
// project's key, a wrong password and an unknown sheet are each refused with the one string
// "not authorized"; the right key joins; suspended refuses with close 4001; resumed joins again.
func TestAdmissionLoop(t *testing.T) {
	srv := StartLeader(t, production)
	a, b := newProject(t, srv), newProject(t, srv)
	sheetA := newSheet(t, srv, a.Project)

	refused := func(t *testing.T, sheet, pw, key string) {
		t.Helper()
		c := Dial(t, srv.WSURL)
		expectRefused(t, c, c.openKey(sheet, pw, key), protocol.ErrMsgNotAuthorized, websocket.StatusPolicyViolation)
	}
	joined := func(t *testing.T, key string) {
		t.Helper()
		c := Dial(t, srv.WSURL)
		if r := c.openKey(sheetA, "pw", key); !r.Success || r.Sheet != sheetA {
			t.Fatalf("open with the project's key = %+v", r)
		}
	}

	t.Run("no key refused", func(t *testing.T) { refused(t, sheetA, "pw", "") })
	t.Run("another project's key refused", func(t *testing.T) { refused(t, sheetA, "pw", b.Key) })
	t.Run("unknown key refused", func(t *testing.T) { refused(t, sheetA, "pw", "rlk_not-a-key") })
	t.Run("right key joins", func(t *testing.T) { joined(t, a.Key) })
	t.Run("wrong password with the right key: same string", func(t *testing.T) { refused(t, sheetA, "wrong", a.Key) })
	t.Run("unknown sheet: same string", func(t *testing.T) { refused(t, UniqueSheet(t), "pw", a.Key) })

	t.Run("suspended refused with 4001, resumed joins", func(t *testing.T) {
		srv.App.Tenants().SetSuspended(a.Project, true)
		c := Dial(t, srv.WSURL)
		expectRefused(t, c, c.openKey(sheetA, "pw", a.Key), protocol.ErrMsgSuspended, protocol.CloseSuspended)
		srv.App.Tenants().SetSuspended(a.Project, false)
		joined(t, a.Key)
	})

	t.Run("refused opens leave no session, closed ones are removed", func(t *testing.T) {
		Eventually(t, "no live sessions once every subtest's socket closed", func() bool {
			return srv.App.Sessions().Len() == 0
		})
	})
}

// TestAdmissionAutocreateIgnoredInProduction is criterion 2: a sheet not in the registry cannot be
// opened in production even with autocreate set — config forces it off at Load, and the key-mode
// router never autocreates even when the flag reaches it.
func TestAdmissionAutocreateIgnoredInProduction(t *testing.T) {
	t.Run("config forces autocreate off outside development", func(t *testing.T) {
		t.Setenv("REDLITE_ENVIRONMENT", "production")
		t.Setenv("REDLITE_SHEET_AUTOCREATE", "true")
		t.Setenv("REDLITE_CLUSTER_TOKEN", "t")
		cfg, err := config.Load(t.TempDir())
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if cfg.SheetAutocreate || !cfg.KeyAdmission() {
			t.Fatalf("production config: autocreate %v admission %q, want false and key", cfg.SheetAutocreate, cfg.Admission)
		}
	})

	t.Run("autocreate forced on after config: still refused", func(t *testing.T) {
		srv := StartLeader(t, production, func(cfg *config.Config) { cfg.SheetAutocreate = true })
		a := newProject(t, srv)
		before := srv.App.Registry().Len()
		c := Dial(t, srv.WSURL)
		expectRefused(t, c, c.openKey(UniqueSheet(t), "pw", a.Key), protocol.ErrMsgNotAuthorized, websocket.StatusPolicyViolation)
		if n := srv.App.Registry().Len(); n != before {
			t.Fatalf("registry grew %d → %d: an unknown sheet was autocreated in production", before, n)
		}
	})
}

// TestAdmissionPasswordModeV1 is criterion 12: REDLITE_ADMISSION=password with autocreate keeps the
// v1 open byte-for-byte — no api_key, no version — and a wrong password still answers
// "invalid password" and closes 1008. A name a revoke dropped is never autocreated (D-025).
func TestAdmissionPasswordModeV1(t *testing.T) {
	srv := StartLeader(t, func(cfg *config.Config) {
		cfg.Environment = "development"
		cfg.Admission = "password"
		cfg.SheetAutocreate = true
	})
	name := UniqueSheet(t)

	c := Dial(t, srv.WSURL)
	c.SendBytes([]byte(`{"action":"open","sheet":"` + name + `","password":"pw"}`))
	resp, _ := c.Reply()
	if !resp.Success || resp.Sheet != name {
		t.Fatalf("v1 open = %+v", resp)
	}
	var st protocol.SheetState
	if err := json.Unmarshal(c.Next(), &st); err != nil || st.Type != protocol.TypeSheetState {
		t.Fatalf("v1 open: expected sheet_state, got %+v (err %v)", st, err)
	}

	w := Dial(t, srv.WSURL)
	w.SendBytes([]byte(`{"action":"open","sheet":"` + name + `","password":"wrong"}`))
	if r, _ := w.Reply(); r.Success || r.Error != protocol.ErrMsgInvalidPassword {
		t.Fatalf("v1 wrong password = %+v, want %q", r, protocol.ErrMsgInvalidPassword)
	}
	if _, err := w.TryNext(5 * time.Second); websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Fatalf("v1 wrong password: err %v, want close 1008", err)
	}

	t.Run("revoked name never autocreated", func(t *testing.T) {
		gone := UniqueSheet(t)
		srv.App.Tenants().MarkRevoked(gone)
		r := Dial(t, srv.WSURL).Do(protocol.Request{Action: protocol.ActionOpen, Sheet: gone, Password: "pw"})
		if r.Success || r.Error != protocol.ErrMsgSheetNotFound {
			t.Fatalf("open of a revoked name = %+v, want %q", r, protocol.ErrMsgSheetNotFound)
		}
		if _, ok := srv.App.Registry().Get(gone); ok {
			t.Fatal("revoked name was autocreated")
		}
	})
}
