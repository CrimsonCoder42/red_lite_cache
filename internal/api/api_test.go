// Covers: internal/api
package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/CrimsonCoder42/red_lite_cache/internal/cache"
	"github.com/CrimsonCoder42/red_lite_cache/internal/sheet"
	"github.com/CrimsonCoder42/red_lite_cache/protocol"
)

const admin = "test-admin-key"

func newTestServer(t *testing.T) (*httptest.Server, *sheet.Registry) {
	t.Helper()
	reg := sheet.NewRegistry(sheet.Options{BcryptCost: 4, Autocreate: false})
	mux := http.NewServeMux()
	New(reg, NewMemStore(), admin, nil).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, reg
}

func call(t *testing.T, srv *httptest.Server, method, path, key string, body any, out any) int {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		json.NewEncoder(&buf).Encode(body)
	}
	req, _ := http.NewRequest(method, srv.URL+path, &buf)
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

func createProject(t *testing.T, srv *httptest.Server, name string) protocol.CreateProjectResponse {
	t.Helper()
	var p protocol.CreateProjectResponse
	if code := call(t, srv, "POST", "/admin/projects", admin, protocol.CreateProjectRequest{Name: name}, &p); code != http.StatusCreated {
		t.Fatalf("create project: status %d", code)
	}
	return p
}

// TestProjectSheetExport is the S6 path a second project takes: admin creates the project and gets
// a key; the project creates its own sheet with that key; the sheet opens with its password; the
// export returns its entries and only to its owner.
func TestProjectSheetExport(t *testing.T) {
	srv, reg := newTestServer(t)
	p := createProject(t, srv, "clinic-b")
	if !strings.HasPrefix(p.APIKey, "rlk_") || p.KeyPrefix != p.APIKey[:8] || p.ProjectID == "" {
		t.Fatalf("bad project response: %+v", p)
	}

	var cs protocol.CreateSheetResponse
	code := call(t, srv, "POST", "/admin/projects/"+p.ProjectID+"/sheets", p.APIKey,
		protocol.CreateSheetRequest{Name: "clinic-b-prod", Password: "pw"}, &cs)
	if code != http.StatusCreated || cs.Sheet != "clinic-b-prod" {
		t.Fatalf("create sheet: %d %+v", code, cs)
	}
	s, created, err := reg.Open("clinic-b-prod", "pw")
	if err != nil || created {
		t.Fatalf("open registry-created sheet: created=%v err=%v", created, err)
	}
	if _, _, err := reg.Open("clinic-b-prod", "wrong"); err == nil {
		t.Fatal("wrong password opened the sheet")
	}
	s.Apply(nil, sheet.Mutation{Kind: sheet.OpSet, Key: "k", Value: json.RawMessage(`{"a":1}`)},
		func(sheet.Result) []byte { return nil })

	var exp protocol.SheetExport
	if code := call(t, srv, "GET", "/admin/sheets/clinic-b-prod/export", p.APIKey, nil, &exp); code != http.StatusOK {
		t.Fatalf("export: %d", code)
	}
	if exp.Count != 1 || string(exp.Entries["k"].Value) != `{"a":1}` || exp.ProjectID != p.ProjectID {
		t.Fatalf("export body: %+v", exp)
	}
	if code := call(t, srv, "GET", "/admin/sheets/clinic-b-prod/export", admin, nil, nil); code != http.StatusOK {
		t.Fatalf("admin export: %d", code)
	}

	other := createProject(t, srv, "clinic-c")
	if code := call(t, srv, "GET", "/admin/sheets/clinic-b-prod/export", other.APIKey, nil, nil); code != http.StatusNotFound {
		t.Fatalf("another project's key exported the sheet: %d", code)
	}
	if code := call(t, srv, "POST", "/admin/projects/"+p.ProjectID+"/sheets", other.APIKey,
		protocol.CreateSheetRequest{Name: "x", Password: "pw"}, nil); code != http.StatusNotFound {
		t.Fatalf("another project's key created a sheet in this project: %d", code)
	}
}

func TestAuthAndValidation(t *testing.T) {
	srv, _ := newTestServer(t)
	p := createProject(t, srv, "p1")
	cases := []struct {
		name, method, path, key string
		body                    any
		want                    int
	}{
		{"no key", "POST", "/admin/projects", "", protocol.CreateProjectRequest{Name: "x"}, http.StatusUnauthorized},
		{"wrong key", "POST", "/admin/projects", "nope", protocol.CreateProjectRequest{Name: "x"}, http.StatusUnauthorized},
		{"project key cannot create projects", "POST", "/admin/projects", p.APIKey, protocol.CreateProjectRequest{Name: "x"}, http.StatusUnauthorized},
		{"empty name", "POST", "/admin/projects", admin, protocol.CreateProjectRequest{}, http.StatusBadRequest},
		{"duplicate name", "POST", "/admin/projects", admin, protocol.CreateProjectRequest{Name: "p1"}, http.StatusConflict},
		{"bad project id", "POST", "/admin/projects/not-a-uuid/sheets", admin, protocol.CreateSheetRequest{Name: "s", Password: "p"}, http.StatusBadRequest},
		{"unknown project", "POST", "/admin/projects/00000000-0000-0000-0000-000000000001/sheets", admin, protocol.CreateSheetRequest{Name: "s", Password: "p"}, http.StatusNotFound},
		{"missing password", "POST", "/admin/projects/" + p.ProjectID + "/sheets", admin, protocol.CreateSheetRequest{Name: "s"}, http.StatusBadRequest},
		{"unknown fields", "POST", "/admin/projects", admin, struct{ Bogus int }{1}, http.StatusBadRequest},
		{"unknown sheet export", "GET", "/admin/sheets/none/export", admin, nil, http.StatusNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := call(t, srv, c.method, c.path, c.key, c.body, nil); got != c.want {
				t.Fatalf("status %d, want %d", got, c.want)
			}
		})
	}
	call(t, srv, "POST", "/admin/projects/"+p.ProjectID+"/sheets", admin, protocol.CreateSheetRequest{Name: "dup", Password: "p"}, nil)
	if got := call(t, srv, "POST", "/admin/projects/"+p.ProjectID+"/sheets", admin, protocol.CreateSheetRequest{Name: "dup", Password: "p"}, nil); got != http.StatusConflict {
		t.Fatalf("duplicate sheet: %d", got)
	}
}

// TestEmptyAdminKeyRefusesEverything: with REDLITE_ADMIN_KEY unset, "Bearer " + "" never matches.
func TestEmptyAdminKeyRefusesEverything(t *testing.T) {
	reg := sheet.NewRegistry(sheet.Options{BcryptCost: 4, Policy: cache.Policy{}})
	mux := http.NewServeMux()
	New(reg, NewMemStore(), "", nil).Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	for _, key := range []string{"", " ", "x"} {
		if got := call(t, srv, "POST", "/admin/projects", key, protocol.CreateProjectRequest{Name: "x"}, nil); got != http.StatusUnauthorized {
			t.Fatalf("key %q: status %d", key, got)
		}
	}
}

func TestKeyAtRestIsHashed(t *testing.T) {
	k, raw, err := newKey()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(k.Hash, []byte(raw)) || len(k.Hash) != 32 || k.Prefix != raw[:8] {
		t.Fatalf("stored key form wrong: %+v", k)
	}
}

// TestReplicaRefusesAdminWrites: on a replica (leader() false) project and sheet creation answer
// 503 not leader — a sheet created in the mirror would vanish at the leader's next snapshot.
// Export still works (a read of the mirror).
func TestReplicaRefusesAdminWrites(t *testing.T) {
	reg := sheet.NewRegistry(sheet.Options{BcryptCost: 4})
	mux := http.NewServeMux()
	New(reg, NewMemStore(), admin, func() bool { return false }).Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	if got := call(t, srv, "POST", "/admin/projects", admin, protocol.CreateProjectRequest{Name: "x"}, nil); got != http.StatusServiceUnavailable {
		t.Fatalf("create project on a replica: %d", got)
	}
	if got := call(t, srv, "POST", "/admin/projects/00000000-0000-0000-0000-000000000000/sheets", admin,
		protocol.CreateSheetRequest{Name: "s", Password: "p"}, nil); got != http.StatusServiceUnavailable {
		t.Fatalf("create sheet on a replica: %d", got)
	}
	reg.Restore(sheet.DefaultProject, "mirrored", []byte("hash"))
	if got := call(t, srv, "GET", "/admin/sheets/mirrored/export", admin, nil, nil); got != http.StatusOK {
		t.Fatalf("export on a replica: %d", got)
	}
}
