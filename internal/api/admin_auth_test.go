// Covers: internal/api admin portal auth (S11 criteria 1–9, 12): bootstrap, login, lockout, cookie,
// sessions, must-change, cross-origin, role matrix, audit actor, rate limits, persistence off
package api

import (
	"bytes"
	"crypto/sha256"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/time/rate"

	"github.com/CrimsonCoder42/red_lite_cache/internal/adminauth"
	"github.com/CrimsonCoder42/red_lite_cache/internal/sheet"
	"github.com/CrimsonCoder42/red_lite_cache/protocol"
)

// TestBootstrapOnce: the admin key creates the first super_admin; it can sign in as super_admin.
func TestBootstrapOnce(t *testing.T) {
	p := newPortal(t, Options{})
	_, me := p.bootstrapSuper(t)
	if me.Role != "super_admin" || me.User == nil || me.User.Email != "root@example.test" || me.MustChangePassword {
		t.Fatalf("me after bootstrap = %+v", me)
	}
	if n := p.count(t, "SELECT count(*) FROM admin_users"); n != 1 {
		t.Fatalf("admin_users rows = %d, want 1", n)
	}
	if len(p.audit.find("admin_bootstrap")) != 1 {
		t.Fatal("no admin_bootstrap audit row")
	}
}

// TestBootstrapConcurrent: 10 simultaneous callers, exactly one 201, nine 410, one user.
func TestBootstrapConcurrent(t *testing.T) {
	p := newPortal(t, Options{})
	codes := make([]int, 10)
	var wg sync.WaitGroup
	for i := range codes {
		wg.Go(func() {
			r := p.do(t, nil, "POST", "/admin/bootstrap", admin, BootstrapRequest{
				Email: "root" + strconv.Itoa(i) + "@example.test", Password: testPassword,
			})
			codes[i] = r.status
		})
	}
	wg.Wait()
	created, gone := 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusCreated:
			created++
		case http.StatusGone:
			gone++
		}
	}
	if created != 1 || gone != 9 {
		t.Fatalf("statuses %v: %d created, %d gone; want 1 and 9", codes, created, gone)
	}
	if n := p.count(t, "SELECT count(*) FROM admin_users"); n != 1 {
		t.Fatalf("admin_users rows = %d, want 1", n)
	}
}

// TestBootstrapNeedsAdminKey: no credential, a wrong key and a signed-in super_admin's cookie are
// all refused 401; only the bearer admin key reaches the handler.
func TestBootstrapNeedsAdminKey(t *testing.T) {
	p := newPortal(t, Options{})
	body := BootstrapRequest{Email: "root@example.test", Password: testPassword}
	if r := p.do(t, nil, "POST", "/admin/bootstrap", "", body); r.status != http.StatusUnauthorized {
		t.Fatalf("no credential: %d", r.status)
	}
	if r := p.do(t, nil, "POST", "/admin/bootstrap", "wrong-key", body); r.status != http.StatusUnauthorized {
		t.Fatalf("wrong key: %d", r.status)
	}
	if n := p.count(t, "SELECT count(*) FROM admin_users"); n != 0 {
		t.Fatalf("a refused bootstrap created %d users", n)
	}
	super, _ := p.bootstrapSuper(t)
	if r := p.do(t, super, "POST", "/admin/bootstrap", "", body); r.status != http.StatusUnauthorized {
		t.Fatalf("cookie caller: %d, want 401 (admin key only)", r.status)
	}
}

// TestBootstrapGoneAfterFirst: every later call is 410, even with a bad body.
func TestBootstrapGoneAfterFirst(t *testing.T) {
	p := newPortal(t, Options{})
	p.bootstrapSuper(t)
	for _, body := range []any{
		BootstrapRequest{Email: "second@example.test", Password: testPassword},
		map[string]string{"nonsense": "x"},
	} {
		if r := p.do(t, nil, "POST", "/admin/bootstrap", admin, body); r.status != http.StatusGone {
			t.Fatalf("second bootstrap: %d %s, want 410", r.status, r.body)
		}
	}
	if n := p.count(t, "SELECT count(*) FROM admin_users"); n != 1 {
		t.Fatalf("admin_users rows = %d, want 1", n)
	}
}

// TestLoginCookieFlags: the session cookie is HttpOnly, Secure (outside development),
// SameSite=Strict, Path=/, 12 h; only its sha256 is stored.
func TestLoginCookieFlags(t *testing.T) {
	p := newPortal(t, Options{SecureCookie: true})
	p.do(t, nil, "POST", "/admin/bootstrap", admin, BootstrapRequest{Email: "root@example.test", Password: testPassword})
	r := p.do(t, nil, "POST", "/admin/auth/login", "", LoginRequest{Email: "root@example.test", Password: testPassword})
	if r.status != http.StatusOK {
		t.Fatalf("login: %d %s", r.status, r.body)
	}
	cookies := (&http.Response{Header: r.hdr}).Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies = %d, want 1", len(cookies))
	}
	ck := cookies[0]
	if ck.Name != CookieName || !ck.HttpOnly || !ck.Secure || ck.SameSite != http.SameSiteStrictMode ||
		ck.Path != "/" || ck.MaxAge != int(adminauth.SessionTTL/time.Second) || len(ck.Value) < 40 {
		t.Fatalf("cookie = %+v", ck)
	}
	sum := sha256.Sum256([]byte(ck.Value))
	if n := p.count(t, "SELECT count(*) FROM admin_sessions WHERE token_hash = $1", sum[:]); n != 1 {
		t.Fatal("the session row is not keyed by sha256(cookie)")
	}
	if n := p.count(t, "SELECT count(*) FROM admin_sessions WHERE token_hash = $1", []byte(ck.Value)); n != 0 {
		t.Fatal("the raw cookie value is stored")
	}
}

// TestUnknownEmailSameReply: unknown email, wrong password, disabled user and locked user get the
// byte-identical reply; at a real bcrypt cost the unknown-email path takes as long as a wrong
// password (within 2× either way of the median). Cost 10 (bcrypt.DefaultCost), not 12: both paths
// run exactly one compare at the configured cost, so the comparison does not depend on it, and
// cost 12 under -race made this one test take 71 s.
func TestUnknownEmailSameReply(t *testing.T) {
	p := newPortal(t, Options{BcryptCost: bcrypt.DefaultCost})
	super, _ := p.bootstrapSuper(t)
	_, off := p.newUser(t, super, "off@example.test", "viewer")
	if r := p.do(t, super, "PATCH", "/admin/users/"+off.UserID, "", PatchUserRequest{Disabled: ptr(true)}); r.status != http.StatusOK {
		t.Fatalf("disable: %d", r.status)
	}
	try := func(email, pw string) (reply, time.Duration) {
		start := time.Now()
		r := p.do(t, nil, "POST", "/admin/auth/login", "", LoginRequest{Email: email, Password: pw})
		return r, time.Since(start)
	}
	ref, _ := try("root@example.test", "wrong-password-123456")
	if ref.status != http.StatusUnauthorized {
		t.Fatalf("wrong password: %d", ref.status)
	}
	for _, c := range []struct{ email, pw string }{
		{"nobody@example.test", testPassword},
		{"off@example.test", testPassword},
	} {
		if r, _ := try(c.email, c.pw); r.status != ref.status || !bytes.Equal(r.body, ref.body) {
			t.Fatalf("%s: %d %q, want %d %q", c.email, r.status, r.body, ref.status, ref.body)
		}
	}
	var unknown, wrong []time.Duration
	for range 3 {
		_, d := try("nobody@example.test", testPassword)
		unknown = append(unknown, d)
		_, d = try("off@example.test", "wrong-password-123456") // disabled: compare, no counter
		wrong = append(wrong, d)
	}
	slices.Sort(unknown)
	slices.Sort(wrong)
	mu, mw := unknown[1], wrong[1]
	if mu*2 < mw || mw*2 < mu {
		t.Fatalf("median unknown-email %v vs wrong-password %v: outside 2×", mu, mw)
	}
	// locked: five wrong passwords lock root; then the right one reads the same as a wrong one
	for range adminauth.LockAfter {
		try("root@example.test", "wrong-password-123456")
	}
	if r, _ := try("root@example.test", testPassword); r.status != ref.status || !bytes.Equal(r.body, ref.body) {
		t.Fatalf("locked user with the right password: %d %q", r.status, r.body)
	}
}

// TestLoginRecordLocksAtFive: four failures do not lock and a success resets them; five
// consecutive failures lock for 15 minutes even against the right password.
func TestLoginRecordLocksAtFive(t *testing.T) {
	p := newPortal(t, Options{})
	p.bootstrapSuper(t)
	bad := LoginRequest{Email: "root@example.test", Password: "wrong-password-123456"}
	good := LoginRequest{Email: "root@example.test", Password: testPassword}
	for range adminauth.LockAfter - 1 {
		p.do(t, nil, "POST", "/admin/auth/login", "", bad)
	}
	if r := p.do(t, nil, "POST", "/admin/auth/login", "", good); r.status != http.StatusOK {
		t.Fatalf("after 4 failures the right password: %d", r.status)
	}
	if n := p.count(t, "SELECT failed_logins FROM admin_users"); n != 0 {
		t.Fatalf("a success left failed_logins = %d", n)
	}
	for range adminauth.LockAfter {
		p.do(t, nil, "POST", "/admin/auth/login", "", bad)
	}
	if r := p.do(t, nil, "POST", "/admin/auth/login", "", good); r.status != http.StatusUnauthorized {
		t.Fatalf("after 5 failures the right password: %d, want 401 (locked)", r.status)
	}
	mins := p.count(t, "SELECT round(extract(epoch FROM locked_until - now()) / 60)::int FROM admin_users")
	if mins < 14 || mins > 15 {
		t.Fatalf("locked for %d minutes, want 15", mins)
	}
	if len(p.audit.find("admin_locked")) != 1 {
		t.Fatal("no admin_locked audit row")
	}
	p.exec(t, "UPDATE admin_users SET locked_until = now() - interval '1 second'")
	if r := p.do(t, nil, "POST", "/admin/auth/login", "", good); r.status != http.StatusOK {
		t.Fatalf("after the lock expired: %d", r.status)
	}
}

// TestSessionCheckIdleAbsoluteDisabled: a session idle 30 min, past its 12 h end, or of a disabled
// user is refused 401.
func TestSessionCheckIdleAbsoluteDisabled(t *testing.T) {
	p := newPortal(t, Options{})
	super, _ := p.bootstrapSuper(t)
	viewer, v := p.newUser(t, super, "v@example.test", "viewer")
	me := func(c *http.Client) int { return p.do(t, c, "GET", "/admin/auth/me", "", nil).status }
	if me(super) != http.StatusOK || me(viewer) != http.StatusOK {
		t.Fatal("fresh sessions refused")
	}
	hash := func(c *http.Client) []byte { s := sha256.Sum256([]byte(p.sessionCookie(c))); return s[:] }

	p.exec(t, "UPDATE admin_sessions SET last_seen_at = now() - interval '31 minutes' WHERE token_hash = $1", hash(super))
	if got := me(super); got != http.StatusUnauthorized {
		t.Fatalf("idle 31 min: %d", got)
	}
	super, _ = p.login(t, "root@example.test", testPassword)
	p.exec(t, "UPDATE admin_sessions SET expires_at = now() - interval '1 second' WHERE token_hash = $1", hash(super))
	if got := me(super); got != http.StatusUnauthorized {
		t.Fatalf("past its absolute end: %d", got)
	}
	p.exec(t, "UPDATE admin_users SET disabled_at = now() WHERE id = $1", uuid.MustParse(v.UserID))
	if got := me(viewer); got != http.StatusUnauthorized {
		t.Fatalf("disabled user: %d", got)
	}
}

// TestLogoutRevokes: after logout the old cookie is refused even if replayed.
func TestLogoutRevokes(t *testing.T) {
	p := newPortal(t, Options{})
	super, _ := p.bootstrapSuper(t)
	old := p.sessionCookie(super)
	if r := p.do(t, super, "POST", "/admin/auth/logout", "", nil); r.status != http.StatusNoContent {
		t.Fatalf("logout: %d %s", r.status, r.body)
	}
	if p.sessionCookie(super) != "" {
		t.Fatal("logout did not clear the cookie")
	}
	if r := p.do(t, nil, "GET", "/admin/auth/me", "", nil, "Cookie", CookieName+"="+old); r.status != http.StatusUnauthorized {
		t.Fatalf("replayed cookie after logout: %d", r.status)
	}
	if len(p.audit.find("admin_logout")) != 1 {
		t.Fatal("no admin_logout audit row")
	}
}

// TestMustChangeBlocks: with a temporary password only password, me and logout answer.
func TestMustChangeBlocks(t *testing.T) {
	p := newPortal(t, Options{})
	super, _ := p.bootstrapSuper(t)
	r := p.do(t, super, "POST", "/admin/users", "", CreateUserRequest{Email: "new@example.test", Role: "admin"})
	var cu CreateUserResponse
	r.decode(t, &cu)
	c, me := p.login(t, "new@example.test", cu.TemporaryPassword)
	if !me.MustChangePassword {
		t.Fatal("login with a temporary password: must_change_password false")
	}
	if r := p.do(t, c, "GET", "/admin/projects", "", nil); r.status != http.StatusForbidden || !strings.Contains(string(r.body), "password change required") {
		t.Fatalf("projects before the change: %d %s", r.status, r.body)
	}
	if r := p.do(t, c, "GET", "/admin/auth/me", "", nil); r.status != http.StatusOK {
		t.Fatalf("me before the change: %d", r.status)
	}
	if r := p.do(t, c, "POST", "/admin/auth/password", "", PasswordChangeRequest{CurrentPassword: cu.TemporaryPassword, NewPassword: testPassword}); r.status != http.StatusNoContent {
		t.Fatalf("change: %d %s", r.status, r.body)
	}
	if r := p.do(t, c, "GET", "/admin/projects", "", nil); r.status != http.StatusOK {
		t.Fatalf("projects after the change: %d %s", r.status, r.body)
	}
}

// TestCrossOriginPostRefused: a cookie caller's unsafe request marked cross-site (Sec-Fetch-Site)
// or from another Origin is 403; the same request same-origin passes; bearer callers are not checked.
func TestCrossOriginPostRefused(t *testing.T) {
	p := newPortal(t, Options{})
	super, _ := p.bootstrapSuper(t)
	body := protocol.CreateProjectRequest{Name: "x-origin"}
	for _, h := range [][]string{{"Sec-Fetch-Site", "cross-site"}, {"Origin", "https://evil.example"}} {
		if r := p.do(t, super, "POST", "/admin/projects", "", body, h...); r.status != http.StatusForbidden {
			t.Fatalf("%s: %d %s, want 403", h[0], r.status, r.body)
		}
	}
	if n := p.count(t, "SELECT count(*) FROM projects WHERE name = 'x-origin'"); n != 0 {
		t.Fatal("a refused cross-origin request created the project")
	}
	if r := p.do(t, super, "POST", "/admin/projects", "", body, "Sec-Fetch-Site", "same-origin"); r.status != http.StatusCreated {
		t.Fatalf("same-origin: %d %s", r.status, r.body)
	}
	if r := p.do(t, nil, "POST", "/admin/projects", admin, protocol.CreateProjectRequest{Name: "x-origin-2"}, "Origin", "https://evil.example"); r.status != http.StatusCreated {
		t.Fatalf("bearer with a foreign Origin: %d", r.status)
	}
}

// TestRoleMatrix: every route of the spec's table refuses a lower role (403) and no credential
// (401), and lets its role and above past the guard.
func TestRoleMatrix(t *testing.T) {
	p := newPortal(t, Options{})
	super, _ := p.bootstrapSuper(t)
	viewer, _ := p.newUser(t, super, "viewer@example.test", "viewer")
	adm, _ := p.newUser(t, super, "admin@example.test", "admin")
	id := uuid.NewString()
	routes := []struct {
		method, path string
		min          adminauth.Role
	}{
		{"GET", "/admin/projects", adminauth.RoleViewer},
		{"GET", "/admin/projects/" + id, adminauth.RoleViewer},
		{"GET", "/admin/sessions", adminauth.RoleViewer},
		{"GET", "/admin/sheets/s/sessions", adminauth.RoleViewer},
		{"GET", "/admin/stats", adminauth.RoleViewer},
		{"GET", "/admin/audit", adminauth.RoleViewer},
		{"GET", "/admin/auth/me", adminauth.RoleViewer},
		{"POST", "/admin/projects", adminauth.RoleAdmin},
		{"POST", "/admin/projects/" + id + "/sheets", adminauth.RoleAdmin},
		{"POST", "/admin/projects/" + id + "/keys", adminauth.RoleAdmin},
		{"POST", "/admin/projects/" + id + "/suspend", adminauth.RoleAdmin},
		{"POST", "/admin/projects/" + id + "/resume", adminauth.RoleAdmin},
		{"POST", "/admin/projects/" + id + "/revoke", adminauth.RoleAdmin},
		{"POST", "/admin/keys/" + id + "/rotate", adminauth.RoleAdmin},
		{"DELETE", "/admin/keys/" + id, adminauth.RoleAdmin},
		{"DELETE", "/admin/sessions/" + id, adminauth.RoleAdmin},
		{"GET", "/admin/sheets/s/export", adminauth.RoleSuperAdmin},
		{"GET", "/admin/users", adminauth.RoleSuperAdmin},
		{"POST", "/admin/users", adminauth.RoleSuperAdmin},
		{"PATCH", "/admin/users/" + id, adminauth.RoleSuperAdmin},
		{"POST", "/admin/users/" + id + "/reset-password", adminauth.RoleSuperAdmin},
	}
	callers := []struct {
		name string
		c    *http.Client
		role adminauth.Role
	}{{"viewer", viewer, adminauth.RoleViewer}, {"admin", adm, adminauth.RoleAdmin}, {"super_admin", super, adminauth.RoleSuperAdmin}}
	for _, rt := range routes {
		if r := p.do(t, newClient(), rt.method, rt.path, "", map[string]string{}); r.status != http.StatusUnauthorized {
			t.Errorf("%s %s without a credential: %d, want 401", rt.method, rt.path, r.status)
		}
		for _, c := range callers {
			r := p.do(t, c.c, rt.method, rt.path, "", map[string]string{})
			refused := r.status == http.StatusForbidden || r.status == http.StatusUnauthorized
			if c.role < rt.min && r.status != http.StatusForbidden {
				t.Errorf("%s %s as %s: %d, want 403", rt.method, rt.path, c.name, r.status)
			}
			if c.role >= rt.min && refused {
				t.Errorf("%s %s as %s: %d %s, want past the guard", rt.method, rt.path, c.name, r.status, r.body)
			}
		}
	}
}

// TestAuditActorIsUser: a signed-in user's mutation is audited as user:<uuid>, the key's as admin.
func TestAuditActorIsUser(t *testing.T) {
	p := newPortal(t, Options{})
	super, me := p.bootstrapSuper(t)
	if r := p.do(t, super, "POST", "/admin/projects", "", protocol.CreateProjectRequest{Name: "by-user"}); r.status != http.StatusCreated {
		t.Fatalf("create as user: %d", r.status)
	}
	if r := p.do(t, nil, "POST", "/admin/projects", admin, protocol.CreateProjectRequest{Name: "by-key"}); r.status != http.StatusCreated {
		t.Fatalf("create as key: %d", r.status)
	}
	ev := p.audit.find("project_created")
	if len(ev) != 2 || ev[0].Actor != "user:"+me.User.UserID || ev[1].Actor != "admin" {
		t.Fatalf("project_created actors = %+v, want user:%s then admin", ev, me.User.UserID)
	}
}

// TestLoginRateLimitPerIP: the sixth sign-in from one IP within a minute is 429 with Retry-After.
func TestLoginRateLimitPerIP(t *testing.T) {
	p := newPortal(t, Options{Limiter: adminauth.NewLimiter(adminauth.DefaultLimits(), nil)})
	bad := LoginRequest{Email: "nobody@example.test", Password: "wrong-password-123456"}
	for i := range 5 {
		if r := p.do(t, nil, "POST", "/admin/auth/login", "", bad); r.status != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d, want 401", i+1, r.status)
		}
	}
	r := p.do(t, nil, "POST", "/admin/auth/login", "", bad)
	if r.status != http.StatusTooManyRequests {
		t.Fatalf("attempt 6: %d, want 429", r.status)
	}
	if s, err := strconv.Atoi(r.hdr.Get("Retry-After")); err != nil || s < 1 || s > 12 {
		t.Fatalf("Retry-After = %q, want 1..12 s", r.hdr.Get("Retry-After"))
	}
}

// TestPerUserLimit: a signed-in user is held to its own bucket; the bearer key is not.
func TestPerUserLimit(t *testing.T) {
	lim := adminauth.Limits{
		IP:   adminauth.Rate{R: 1000, Burst: 1000},
		User: adminauth.Rate{R: rate.Every(time.Hour), Burst: 3}, MaxEntries: 100,
	}
	p := newPortal(t, Options{Limiter: adminauth.NewLimiter(lim, nil)})
	super, _ := p.bootstrapSuper(t)
	for i := range 3 {
		if r := p.do(t, super, "GET", "/admin/projects", "", nil); r.status != http.StatusOK {
			t.Fatalf("user request %d: %d", i+1, r.status)
		}
	}
	if r := p.do(t, super, "GET", "/admin/projects", "", nil); r.status != http.StatusTooManyRequests {
		t.Fatalf("user request 4: %d, want 429", r.status)
	}
	for i := range 10 {
		if r := p.do(t, nil, "GET", "/admin/projects", admin, nil); r.status != http.StatusOK {
			t.Fatalf("bearer request %d: %d", i+1, r.status)
		}
	}
}

// TestPersistenceOffDegradesOnlyPortal (criterion 12): without an AdminStore the DB routes answer
// 503 and the bearer routes, stats included, work.
func TestPersistenceOffDegradesOnlyPortal(t *testing.T) {
	reg := sheet.NewRegistry(sheet.Options{BcryptCost: 4})
	mux := http.NewServeMux()
	New(reg, NewMemStore(), admin, nil, Options{}).Register(mux)
	p := &portal{srv: newHTTPTest(t, mux)}
	for _, rt := range [][2]string{
		{"POST", "/admin/bootstrap"}, {"POST", "/admin/auth/login"}, {"GET", "/admin/auth/me"},
		{"GET", "/admin/users"}, {"POST", "/admin/users"}, {"GET", "/admin/audit"},
	} {
		if r := p.do(t, nil, rt[0], rt[1], admin, map[string]string{}); r.status != http.StatusServiceUnavailable {
			t.Errorf("%s %s: %d, want 503", rt[0], rt[1], r.status)
		}
	}
	for _, path := range []string{"/admin/projects", "/admin/sessions", "/admin/stats"} {
		if r := p.do(t, nil, "GET", path, admin, nil); r.status != http.StatusOK {
			t.Errorf("GET %s: %d, want 200", path, r.status)
		}
	}
}

func ptr[T any](v T) *T { return &v }

// TestPortalInputEdges (checker findings): a NUL byte in an email or name is a 4xx, never a 500;
// a bootstrap whose email a bearer-created user holds is 409 and leaves bootstrap open.
func TestPortalInputEdges(t *testing.T) {
	p := newPortal(t, Options{})
	if r := p.do(t, nil, "POST", "/admin/auth/login", "", LoginRequest{Email: "a\x00@x.test", Password: testPassword}); r.status != http.StatusUnauthorized {
		t.Fatalf("login with a NUL email: %d %s, want 401", r.status, r.body)
	}
	if r := p.do(t, nil, "POST", "/admin/users", admin, CreateUserRequest{Email: "taken@example.test", Name: "n\x00", Role: "viewer"}); r.status != http.StatusBadRequest {
		t.Fatalf("user with a NUL name: %d, want 400", r.status)
	}
	if r := p.do(t, nil, "POST", "/admin/users", admin, CreateUserRequest{Email: "taken@example.test", Role: "viewer"}); r.status != http.StatusCreated {
		t.Fatalf("create by key: %d", r.status)
	}
	if r := p.do(t, nil, "POST", "/admin/bootstrap", admin, BootstrapRequest{Email: "TAKEN@example.test", Password: testPassword}); r.status != http.StatusConflict {
		t.Fatalf("bootstrap with a taken email: %d %s, want 409", r.status, r.body)
	}
	r := p.do(t, nil, "POST", "/admin/bootstrap", admin, BootstrapRequest{Email: "root@example.test", Password: testPassword})
	var me MeResponse
	r.decode(t, &me)
	if r.status != http.StatusCreated || me.User == nil || me.User.CreatedAt == "" {
		t.Fatalf("bootstrap after the 409: %d %s", r.status, r.body)
	}
}

// TestAdminKeySkipsIPLimit (checker finding): the bearer admin key is not held to the per-IP
// bucket (the peer pages up to 25 requests per listing); a wrong key still is.
func TestAdminKeySkipsIPLimit(t *testing.T) {
	lim := adminauth.Limits{IP: adminauth.Rate{R: rate.Every(time.Hour), Burst: 2}, MaxEntries: 100}
	p := newPortal(t, Options{Limiter: adminauth.NewLimiter(lim, nil)})
	for i := range 30 {
		if r := p.do(t, nil, "GET", "/admin/sessions", admin, nil); r.status != http.StatusOK {
			t.Fatalf("admin key request %d: %d", i+1, r.status)
		}
	}
	codes := []int{}
	for range 3 {
		codes = append(codes, p.do(t, nil, "GET", "/admin/sessions", "wrong-key", nil).status)
	}
	if codes[0] != http.StatusUnauthorized || codes[2] != http.StatusTooManyRequests {
		t.Fatalf("wrong key: %v, want 401 … 429", codes)
	}
}
