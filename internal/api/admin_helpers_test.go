package api

// Helpers for the admin portal tests (S11): a fresh Postgres schema per test migrated with
// db.Migrate (the internal/persist/db_test.go testStore pattern), an httptest server over the admin
// mux, and a cookie client. Every request is HTTP against the server: the same calls would drive
// another implementation of the admin port.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"

	"github.com/CrimsonCoder42/red_lite_cache/db"
	"github.com/CrimsonCoder42/red_lite_cache/internal/config"
	"github.com/CrimsonCoder42/red_lite_cache/internal/sheet"
	"github.com/CrimsonCoder42/red_lite_cache/internal/tenant"
)

const testPassword = "correct-horse-battery-staple"

// portalDB migrates a fresh schema on REDLITE_TEST_DATABASE_URL and returns a Store bound to it.
// Skips without the variable.
func portalDB(t *testing.T) *db.Store {
	t.Helper()
	base := config.TestDatabaseURL()
	if base == "" {
		t.Skip("SKIPPED: persist (no REDLITE_TEST_DATABASE_URL)")
	}
	ctx := t.Context()
	schema := fmt.Sprintf("rl_portal_%d", time.Now().UnixNano())
	conn, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := conn.Exec(cctx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Errorf("drop schema %s: %v", schema, err)
		}
		conn.Close(cctx)
	})
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	if err := db.Migrate(u.String()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := db.OpenPool(ctx, u.String(), 1, 8)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return db.NewStore(pool)
}

// recAuditor records audit events in memory.
type recAuditor struct {
	mu sync.Mutex
	ev []tenant.Event
}

func (a *recAuditor) Audit(e tenant.Event) { a.mu.Lock(); a.ev = append(a.ev, e); a.mu.Unlock() }

func (a *recAuditor) find(action string) []tenant.Event {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []tenant.Event
	for _, e := range a.ev {
		if e.Action == action {
			out = append(out, e)
		}
	}
	return out
}

type portal struct {
	srv   *httptest.Server
	store *db.Store
	audit *recAuditor
}

// newPortal starts the admin API over Postgres with bcrypt at MinCost and no rate limits unless
// opts says otherwise.
func newPortal(t *testing.T, opts Options) *portal {
	t.Helper()
	store := portalDB(t)
	rec := &recAuditor{}
	if opts.Auditor == nil {
		opts.Auditor = rec
	}
	if opts.BcryptCost == 0 {
		opts.BcryptCost = bcrypt.MinCost
	}
	opts.Admin = NewAdminDB(store)
	reg := sheet.NewRegistry(sheet.Options{BcryptCost: 4})
	mux := http.NewServeMux()
	New(reg, NewDBStore(store), admin, nil, opts).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &portal{srv: srv, store: store, audit: rec}
}

type reply struct {
	status int
	body   []byte
	hdr    http.Header
}

func (r reply) decode(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.body, v); err != nil {
		t.Fatalf("decode %s: %v", r.body, err)
	}
}

// do sends one request; key "" sends no Authorization header; hdr adds headers.
func (p *portal) do(t *testing.T, c *http.Client, method, path, key string, body any, hdr ...string) reply {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, p.srv.URL+path, rd)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	if c == nil {
		c = http.DefaultClient
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return reply{status: resp.StatusCode, body: b, hdr: resp.Header}
}

func newClient() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar}
}

// bootstrapSuper creates the first super_admin and returns a signed-in client.
func (p *portal) bootstrapSuper(t *testing.T) (*http.Client, MeResponse) {
	t.Helper()
	if r := p.do(t, nil, "POST", "/admin/bootstrap", admin, BootstrapRequest{Email: "root@example.test", Name: "Root", Password: testPassword}); r.status != http.StatusCreated {
		t.Fatalf("bootstrap: %d %s", r.status, r.body)
	}
	return p.login(t, "root@example.test", testPassword)
}

func (p *portal) login(t *testing.T, email, pw string) (*http.Client, MeResponse) {
	t.Helper()
	c := newClient()
	r := p.do(t, c, "POST", "/admin/auth/login", "", LoginRequest{Email: email, Password: pw})
	if r.status != http.StatusOK {
		t.Fatalf("login %s: %d %s", email, r.status, r.body)
	}
	var me MeResponse
	r.decode(t, &me)
	return c, me
}

// newUser has super create a user of role, signs it in with the temporary password and changes
// the password, returning a client with must_change_password cleared.
func (p *portal) newUser(t *testing.T, super *http.Client, email, role string) (*http.Client, UserInfo) {
	t.Helper()
	r := p.do(t, super, "POST", "/admin/users", "", CreateUserRequest{Email: email, Name: role, Role: role})
	if r.status != http.StatusCreated {
		t.Fatalf("create %s: %d %s", role, r.status, r.body)
	}
	var cu CreateUserResponse
	r.decode(t, &cu)
	c, _ := p.login(t, email, cu.TemporaryPassword)
	if r := p.do(t, c, "POST", "/admin/auth/password", "", PasswordChangeRequest{CurrentPassword: cu.TemporaryPassword, NewPassword: testPassword}); r.status != http.StatusNoContent {
		t.Fatalf("password change %s: %d %s", email, r.status, r.body)
	}
	return c, cu.User
}

// exec runs test-only SQL on the test schema (moving a session's clock, reading a row).
func (p *portal) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := p.store.Pool().Exec(t.Context(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

func (p *portal) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := p.store.Pool().QueryRow(t.Context(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	return n
}

// sessionCookie is the client's session cookie value for the server ("" when none).
func (p *portal) sessionCookie(c *http.Client) string {
	u, _ := url.Parse(p.srv.URL)
	for _, ck := range c.Jar.Cookies(u) {
		if ck.Name == CookieName {
			return ck.Value
		}
	}
	return ""
}

func newHTTPTest(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}
