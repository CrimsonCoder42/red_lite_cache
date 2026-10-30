// Covers: backendAdvo cache layer
package conformance

// The conformance cluster: one leader, ONE sheet, three readers = backendAdvo's three API instances
// (CACHE_SHEET is shared, cmd/api/main.go:112-120), each wrapped in the consumer Store, and one
// FakeDB every instance reads through. Every C test runs under both admission modes (cfModes): the
// "key" runs are C13 (KeyModeParity) — the same test code against a production-shaped server.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/CrimsonCoder42/red_lite_cache/internal/config"
	"github.com/CrimsonCoder42/red_lite_cache/protocol"
	"github.com/CrimsonCoder42/red_lite_cache/reader"
	"github.com/CrimsonCoder42/red_lite_cache/testing/integration"
)

const (
	cfAdminKey     = "cf-admin-key"
	cfClusterToken = "cf-cluster-token"
)

// cfMode is one admission mode.
type cfMode struct {
	name string
	key  bool // true = C13: production, key admission, no autocreate, project + key via /admin
}

var cfModes = []cfMode{{name: "password"}, {name: "key_C13", key: true}}

// cfKeyMode is production-shaped admission.
// after testing/integration/tenancy_api_test.go:22-28 (tnKeyMode): same five fields.
func cfKeyMode(c *config.Config) {
	c.AdminKey = cfAdminKey
	c.SheetAutocreate = false
	c.Admission = "key"
	c.Environment = "production"
	c.ClusterToken = cfClusterToken
}

// cfForModes runs body once per admission mode as a subtest.
func cfForModes(t *testing.T, body func(t *testing.T, m cfMode)) {
	for _, m := range cfModes {
		t.Run(m.name, func(t *testing.T) { body(t, m) })
	}
}

// cfCluster is one leader, one sheet, three instances and the database.
type cfCluster struct {
	t      *testing.T
	srv    *integration.Server
	sheet  string
	apiKey string
	st     [3]*Store
	db     *FakeDB
	nb     atomic.Int64 // barrier counter
}

// cfStart boots a leader for mode m (srvMods applied after the mode's), creates the sheet (key
// mode: a project and a sheet through /admin), and connects three readers to it.
func cfStart(t *testing.T, m cfMode, srvMods ...func(*config.Config)) *cfCluster {
	t.Helper()
	mods := []func(*config.Config){}
	if m.key {
		mods = append(mods, cfKeyMode)
	}
	mods = append(mods, srvMods...)
	c := &cfCluster{t: t, srv: integration.StartLeader(t, mods...), db: NewFakeDB()}
	if m.key {
		c.apiKey, c.sheet = cfProject(t, c.srv)
	} else {
		c.sheet = integration.UniqueSheet(t)
	}
	for i := range c.st {
		c.st[i] = c.storeAt(c.srv.WSURL)
	}
	return c
}

// storeAt is one more instance on the cluster's sheet, dialling wsURL (the leader or a proxy).
func (c *cfCluster) storeAt(wsURL string, mods ...func(*reader.Config)) *Store {
	c.t.Helper()
	all := append([]func(*reader.Config){func(rc *reader.Config) { rc.APIKey = c.apiKey }}, mods...)
	return &Store{R: integration.NewReaderAt(c.t, wsURL, "", c.sheet, all...)}
}

// cfProject creates a project and one sheet (password "pw") through the management API and
// returns the project's API key and the sheet name.
// after testing/integration/control_test.go:42-53 (projectWithSheet): same two calls.
func cfProject(t *testing.T, srv *integration.Server) (string, string) {
	t.Helper()
	var p protocol.CreateProjectResponse
	if code := cfAdmin(t, srv, "/admin/projects", cfAdminKey, protocol.CreateProjectRequest{Name: integration.UniqueSheet(t)}, &p); code != http.StatusCreated {
		t.Fatalf("create project: %d", code)
	}
	name := integration.UniqueSheet(t)
	if code := cfAdmin(t, srv, "/admin/projects/"+p.ProjectID+"/sheets", p.APIKey, protocol.CreateSheetRequest{Name: name, Password: "pw"}, nil); code != http.StatusCreated {
		t.Fatalf("create sheet: %d", code)
	}
	return p.APIKey, name
}

// cfAdmin POSTs body to srv's management API with key as bearer.
// after testing/integration/control_test.go:20-39 (adminDo): POST only.
func cfAdmin(t *testing.T, srv *integration.Server, path, key string, body, out any) int {
	t.Helper()
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(body); err != nil {
		t.Fatalf("encode: %v", err)
	}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+path, &buf)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	if out != nil {
		json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

// barrier writes a marker through instance 0 and waits until every given instance (default: all
// three) holds it. Broadcasts reach a reader in the sheet's order, so after barrier every write
// acknowledged to instance 0 before it has been applied on those instances too.
func (c *cfCluster) barrier(on ...*Store) {
	c.t.Helper()
	if len(on) == 0 {
		on = c.st[:]
	}
	n := c.nb.Add(1)
	key := fmt.Sprintf("conformance:barrier:%d", n)
	if err := c.st[0].R.SetWithTTL(context.Background(), key, n, 0); err != nil {
		c.t.Fatalf("barrier set: %v", err)
	}
	for i, s := range on {
		integration.Eventually(c.t, fmt.Sprintf("instance %d applied barrier %d", i, n), func() bool {
			var got int64
			return s.R.Get(key, &got) && got == n
		})
	}
}

// loadRows is the loader every read-through in these tests passes.
func (c *cfCluster) loadRows(profile uuid.UUID) func() ([]Row, error) {
	return func() ([]Row, error) { return c.db.Load(profile) }
}

// readList is one instance's read-through of key for profile.
func (c *cfCluster) readList(s *Store, key string, profile uuid.UUID, ttl int) []Row {
	c.t.Helper()
	rows, err := CachedList(s, context.Background(), key, ttl, c.loadRows(profile))
	if err != nil {
		c.t.Fatalf("CachedList %s: %v", key, err)
	}
	return rows
}

// value is the single row's Value, -1 for no rows.
func value(rows []Row) int {
	if len(rows) == 0 {
		return -1
	}
	return rows[0].Value
}

// expectLoads fails unless the database was hit exactly want times.
func (c *cfCluster) expectLoads(want int64, why string) {
	c.t.Helper()
	if got := c.db.Loads(); got != want {
		c.t.Fatalf("FakeDB loads = %d, want %d (%s)", got, want, why)
	}
}

// cachedValue reports whether s holds key and, when it does, the first row's value.
func cachedValue(s *Store, key string) (int, bool) {
	var rows []Row
	if !s.R.Get(key, &rows) {
		return 0, false
	}
	return value(rows), true
}

// seenEverywhere waits until every instance holds key with value want.
func (c *cfCluster) seenEverywhere(key string, want int) {
	c.t.Helper()
	for i, s := range c.st {
		integration.Eventually(c.t, fmt.Sprintf("instance %d holds %s = %d", i, key, want), func() bool {
			v, ok := cachedValue(s, key)
			return ok && v == want
		})
	}
}

// neverStale fails if any instance can read key with the value stale.
func (c *cfCluster) neverStale(key string, stale int, step string, on ...*Store) {
	c.t.Helper()
	if len(on) == 0 {
		on = c.st[:]
	}
	for i, s := range on {
		if v, ok := cachedValue(s, key); ok && v == stale {
			c.t.Fatalf("%s: instance %d reads the stale value %d of %s", step, i, stale, key)
		}
	}
}

// putAll sets every key (value = its index) through instance 0, then waits for all three.
func (c *cfCluster) putAll(keys []string) {
	c.t.Helper()
	ctx := context.Background()
	for i, k := range keys {
		if err := c.st[0].R.SetWithTTL(ctx, k, i, 0); err != nil {
			c.t.Fatalf("set %s: %v", k, err)
		}
	}
	c.barrier()
	for i, s := range c.st {
		for _, k := range keys {
			if !s.R.Has(k) {
				c.t.Fatalf("instance %d is missing %s after the barrier", i, k)
			}
		}
	}
}

// expectPresence fails unless, on every instance, exactly the keys in gone are absent and every
// other key in all is present.
func (c *cfCluster) expectPresence(all []string, gone map[string]bool, step string) {
	c.t.Helper()
	c.barrier()
	for i, s := range c.st {
		for _, k := range all {
			if has := s.R.Has(k); has == gone[k] {
				c.t.Fatalf("%s: instance %d Has(%s) = %v, want %v", step, i, k, has, !gone[k])
			}
		}
	}
}
