// Covers: internal/api /admin/projects /admin/projects/{id}/sheets /admin/sheets/{id}/export K14 R11 R12
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/CrimsonCoder42/red_lite_cache/internal/config"
	"github.com/CrimsonCoder42/red_lite_cache/protocol"
)

func adminCall(t *testing.T, srv *Server, method, path, key string, body, out any) int {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		json.NewEncoder(&buf).Encode(body)
	}
	req, _ := http.NewRequest(method, srv.URL+path, &buf)
	req.Header.Set("Authorization", "Bearer "+key)
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

// TestAutocreateOffRegistryPath is the multi-project path with REDLITE_SHEET_AUTOCREATE=false: an
// open of an unknown sheet is refused; the admin creates a project, the project creates its sheet
// with its own key, a reader opens it and writes, and the project exports it. Another project's
// key cannot see it. No code change, no restart.
func TestAutocreateOffRegistryPath(t *testing.T) {
	const admin = "it-admin-key"
	srv := StartLeader(t, func(c *config.Config) { c.SheetAutocreate = false; c.AdminKey = admin })
	name := UniqueSheet(t)

	c := Dial(t, srv.WSURL)
	if r := c.Do(protocol.Request{Action: protocol.ActionOpen, Sheet: name, Password: "pw"}); r.Error != protocol.ErrMsgSheetNotFound {
		t.Fatalf("open of an unregistered sheet = %+v, want sheet not found", r)
	}

	var p protocol.CreateProjectResponse
	if code := adminCall(t, srv, "POST", "/admin/projects", admin, protocol.CreateProjectRequest{Name: name + "-proj"}, &p); code != http.StatusCreated {
		t.Fatalf("create project: %d", code)
	}
	if code := adminCall(t, srv, "POST", "/admin/projects/"+p.ProjectID+"/sheets", p.APIKey,
		protocol.CreateSheetRequest{Name: name, Password: "pw"}, nil); code != http.StatusCreated {
		t.Fatalf("create sheet with the project key: %d", code)
	}

	r := NewReader(t, srv, name)
	if !r.IsConnected() {
		t.Fatal("reader did not open the registry-created sheet")
	}
	if err := r.SetWithTTL(context.Background(), "k", map[string]int{"a": 1}, 5); err != nil {
		t.Fatalf("set: %v", err)
	}
	bad := Dial(t, srv.WSURL)
	if r := bad.Do(protocol.Request{Action: protocol.ActionOpen, Sheet: name, Password: "wrong"}); r.Error != protocol.ErrMsgInvalidPassword {
		t.Fatalf("wrong password = %+v", r)
	}

	var exp protocol.SheetExport
	if code := adminCall(t, srv, "GET", "/admin/sheets/"+name+"/export", p.APIKey, nil, &exp); code != http.StatusOK {
		t.Fatalf("export: %d", code)
	}
	if exp.Count != 1 || string(exp.Entries["k"].Value) != `{"a":1}` || exp.Entries["k"].ExpiresInMS <= 0 {
		t.Fatalf("export = %+v", exp)
	}

	var other protocol.CreateProjectResponse
	adminCall(t, srv, "POST", "/admin/projects", admin, protocol.CreateProjectRequest{Name: name + "-other"}, &other)
	if code := adminCall(t, srv, "GET", "/admin/sheets/"+name+"/export", other.APIKey, nil, nil); code != http.StatusNotFound {
		t.Fatalf("another project's key exported the sheet: %d", code)
	}
	if code := adminCall(t, srv, "POST", "/admin/projects", "", protocol.CreateProjectRequest{Name: "x"}, nil); code != http.StatusUnauthorized {
		t.Fatalf("no key: %d", code)
	}
}
