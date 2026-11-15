// Covers: internal/api admin users (S11 criteria 10, 11): create with a temporary password,
// super_admin only, disable ends sessions, last super_admin protected, reset forces a change
package api

import (
	"net/http"
	"sync"
	"testing"
)

// TestSuperAdminCreatesUser: 201 with a 24-character temporary password shown once, must change,
// the password signs in; the same email in another case is 409.
func TestSuperAdminCreatesUser(t *testing.T) {
	p := newPortal(t, Options{})
	super, me := p.bootstrapSuper(t)
	r := p.do(t, super, "POST", "/admin/users", "", CreateUserRequest{Email: "Ops@Example.test", Name: "Ops", Role: "admin"})
	if r.status != http.StatusCreated {
		t.Fatalf("create: %d %s", r.status, r.body)
	}
	var cu CreateUserResponse
	r.decode(t, &cu)
	if len(cu.TemporaryPassword) != 24 || !cu.User.MustChangePassword || cu.User.Role != "admin" || cu.User.CreatedBy != me.User.UserID {
		t.Fatalf("create response = %+v", cu)
	}
	if _, m := p.login(t, "ops@example.test", cu.TemporaryPassword); !m.MustChangePassword || m.Role != "admin" {
		t.Fatalf("login with the temporary password: %+v", m)
	}
	if r := p.do(t, super, "POST", "/admin/users", "", CreateUserRequest{Email: "OPS@example.test", Role: "viewer"}); r.status != http.StatusConflict {
		t.Fatalf("duplicate email: %d", r.status)
	}
	var page struct{ Items []UserInfo }
	p.do(t, super, "GET", "/admin/users", "", nil).decode(t, &page)
	if len(page.Items) != 2 {
		t.Fatalf("users listed = %d, want 2", len(page.Items))
	}
	if r := p.do(t, nil, "POST", "/admin/users", admin, CreateUserRequest{Email: "bykey@example.test", Role: "viewer"}); r.status != http.StatusCreated {
		t.Fatalf("create with the admin key: %d %s", r.status, r.body)
	}
	if len(p.audit.find("admin_user_created")) != 2 {
		t.Fatal("admin_user_created audit rows != 2")
	}
}

// TestAdminCannotManageUsers: role admin gets 403 on every user route.
func TestAdminCannotManageUsers(t *testing.T) {
	p := newPortal(t, Options{})
	super, me := p.bootstrapSuper(t)
	adm, _ := p.newUser(t, super, "admin@example.test", "admin")
	for _, rt := range []struct {
		method, path string
		body         any
	}{
		{"GET", "/admin/users", nil},
		{"POST", "/admin/users", CreateUserRequest{Email: "x@example.test", Role: "super_admin"}},
		{"PATCH", "/admin/users/" + me.User.UserID, PatchUserRequest{Disabled: ptr(true)}},
		{"POST", "/admin/users/" + me.User.UserID + "/reset-password", nil},
	} {
		if r := p.do(t, adm, rt.method, rt.path, "", rt.body); r.status != http.StatusForbidden {
			t.Fatalf("%s %s as admin: %d, want 403", rt.method, rt.path, r.status)
		}
	}
	if n := p.count(t, "SELECT count(*) FROM admin_users WHERE disabled_at IS NOT NULL"); n != 0 {
		t.Fatal("an admin disabled a user")
	}
}

// TestDisableKillsSessions: disabling a user ends its live session at once and its sign-in fails.
func TestDisableKillsSessions(t *testing.T) {
	p := newPortal(t, Options{})
	super, _ := p.bootstrapSuper(t)
	v, u := p.newUser(t, super, "v@example.test", "viewer")
	if r := p.do(t, v, "GET", "/admin/projects", "", nil); r.status != http.StatusOK {
		t.Fatalf("before disable: %d", r.status)
	}
	if r := p.do(t, super, "PATCH", "/admin/users/"+u.UserID, "", PatchUserRequest{Disabled: ptr(true)}); r.status != http.StatusOK {
		t.Fatalf("disable: %d %s", r.status, r.body)
	}
	if r := p.do(t, v, "GET", "/admin/projects", "", nil); r.status != http.StatusUnauthorized {
		t.Fatalf("after disable: %d, want 401", r.status)
	}
	if n := p.count(t, "SELECT count(*) FROM admin_sessions s JOIN admin_users u ON u.id = s.user_id WHERE u.email = 'v@example.test'"); n != 0 {
		t.Fatalf("%d sessions of the disabled user remain", n)
	}
	if r := p.do(t, nil, "POST", "/admin/auth/login", "", LoginRequest{Email: "v@example.test", Password: testPassword}); r.status != http.StatusUnauthorized {
		t.Fatalf("login after disable: %d", r.status)
	}
	if r := p.do(t, super, "PATCH", "/admin/users/"+u.UserID, "", PatchUserRequest{Disabled: ptr(false)}); r.status != http.StatusOK {
		t.Fatalf("enable: %d", r.status)
	}
	p.login(t, "v@example.test", testPassword)
}

// TestLastSuperAdminProtected: the only active super_admin cannot demote or disable itself; with
// two, two concurrent cross-demotions leave exactly one.
func TestLastSuperAdminProtected(t *testing.T) {
	p := newPortal(t, Options{})
	super, me := p.bootstrapSuper(t)
	for _, body := range []PatchUserRequest{{Role: ptr("admin")}, {Disabled: ptr(true)}} {
		if r := p.do(t, super, "PATCH", "/admin/users/"+me.User.UserID, "", body); r.status != http.StatusConflict {
			t.Fatalf("last super_admin %+v: %d %s, want 409", body, r.status, r.body)
		}
	}
	other, o := p.newUser(t, super, "two@example.test", "super_admin")
	codes := make([]int, 2)
	var wg sync.WaitGroup
	wg.Go(func() {
		codes[0] = p.do(t, super, "PATCH", "/admin/users/"+o.UserID, "", PatchUserRequest{Role: ptr("viewer")}).status
	})
	wg.Go(func() {
		codes[1] = p.do(t, other, "PATCH", "/admin/users/"+me.User.UserID, "", PatchUserRequest{Role: ptr("viewer")}).status
	})
	wg.Wait()
	// The loser is 409 when both passed the guard, or 403 when the winner committed before the
	// loser's own session check saw its new role; either way exactly one demotion lands.
	ok, refused := 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusOK:
			ok++
		case http.StatusConflict, http.StatusForbidden:
			refused++
		}
	}
	if ok != 1 || refused != 1 {
		t.Fatalf("concurrent demotions: %v, want one 200 and one 409/403", codes)
	}
	if n := p.count(t, "SELECT count(*) FROM admin_users WHERE role = 'super_admin' AND disabled_at IS NULL"); n != 1 {
		t.Fatalf("active super_admins = %d, want 1", n)
	}
}

// TestResetForcesChange: a reset ends the user's sessions, the old password stops working, the
// temporary one signs in with must_change_password set, and clears a lockout.
func TestResetForcesChange(t *testing.T) {
	p := newPortal(t, Options{})
	super, _ := p.bootstrapSuper(t)
	v, u := p.newUser(t, super, "v@example.test", "viewer")
	p.exec(t, "UPDATE admin_users SET failed_logins = 3 WHERE email = 'v@example.test'")
	r := p.do(t, super, "POST", "/admin/users/"+u.UserID+"/reset-password", "", nil)
	if r.status != http.StatusOK {
		t.Fatalf("reset: %d %s", r.status, r.body)
	}
	var rp ResetPasswordResponse
	r.decode(t, &rp)
	if r := p.do(t, v, "GET", "/admin/auth/me", "", nil); r.status != http.StatusUnauthorized {
		t.Fatalf("old session after reset: %d", r.status)
	}
	if r := p.do(t, nil, "POST", "/admin/auth/login", "", LoginRequest{Email: "v@example.test", Password: testPassword}); r.status != http.StatusUnauthorized {
		t.Fatalf("old password after reset: %d", r.status)
	}
	if _, me := p.login(t, "v@example.test", rp.TemporaryPassword); !me.MustChangePassword {
		t.Fatal("temporary password sign-in: must_change_password false")
	}
	if len(p.audit.find("admin_password_reset")) != 1 {
		t.Fatal("no admin_password_reset audit row")
	}
}
