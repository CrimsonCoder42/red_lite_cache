package api

// Admin portal auth (S11, doc/specs/s11_admin_portal.md): the per-route guard, the session cookie,
// bootstrap, sign-in, sign-out, me and password change. Passwords are bcrypt in Go (D1); lockout,
// sessions and the bootstrap latch are SQL (migration 000003). The bearer admin key keeps working
// as super_admin with actor "admin" (bootstrap.sh, the peer node, break-glass).

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/CrimsonCoder42/red_lite_cache/internal/adminauth"
	"github.com/CrimsonCoder42/red_lite_cache/internal/logger"
	"github.com/CrimsonCoder42/red_lite_cache/internal/tenant"
)

// CookieName is the session cookie (D2).
const CookieName = "redlite_admin"

// errSignIn is the one sign-in failure reply: unknown email, wrong password, disabled and locked
// users all read the same (criterion 2: no account enumeration, no lock oracle).
const errSignIn = "sign-in failed"

const (
	maxEmailLen = 254
	maxNameLen  = 200
)

// callerKey carries the guard's resolved caller to the handler.
type callerKey struct{}

// need is one route's row of the spec's route table.
type need struct {
	role         adminauth.Role // minimum operator role
	project      bool           // a project API key is accepted too (S6 routes)
	db           bool           // needs the AdminStore: 503 without persistence
	login        bool           // also takes a token from the per-IP sign-in bucket
	keyOnly      bool           // only the bearer admin key (bootstrap)
	cookieOnly   bool           // only a session cookie (logout, password change)
	public       bool           // no credential at all (login)
	mustChangeOK bool           // answers while the user must change the password
}

// initPortal sets the S11 fields from opts.
func (s *Server) initPortal(opts Options) {
	s.admins, s.limiter, s.trustProxy, s.secureCookie = opts.Admin, opts.Limiter, opts.TrustedProxy, opts.SecureCookie
	s.stats, s.startedAt = opts.Stats, time.Now()
	s.bcryptCost = opts.BcryptCost
	if s.bcryptCost == 0 {
		s.bcryptCost = adminauth.BcryptCost
	}
	s.cop = http.NewCrossOriginProtection()
}

// dummy is the hash an unknown email is compared against, at the real cost, so that path costs
// one bcrypt too (criterion 2). Made on the first sign-in of any kind (not in New: every instance
// start would pay a cost-12 bcrypt), so the first known and the first unknown email pay the same.
func (s *Server) dummy() string {
	s.dummyOnce.Do(func() {
		pw, err := adminauth.TempPassword()
		if err == nil {
			s.dummyHash, err = adminauth.HashPassword(pw, s.bcryptCost)
		}
		if err != nil {
			logger.L.Error("API: dummy hash failed", "err", err) // the unknown-email path then skips bcrypt
		}
	})
	return s.dummyHash
}

// guard is every admin route's front: per-IP limit (and the sign-in bucket), persistence check,
// cross-origin check for browser requests on unsafe methods, credential, role, must-change,
// per-user limit. Order: cheap refusals first, so unauthenticated traffic costs one map lookup (PLAN.md §8).
func (s *Server) guard(n need, h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := adminauth.ClientIP(r, s.trustProxy)
		// The admin key (a sha256 compare, no I/O) skips the per-IP bucket: the peer node pages up
		// to 25 requests per merged listing (sessions.go peerPages). A wrong key is still limited.
		if _, isKey := s.adminKeyCaller(r); !isKey && !s.allow(w, r, adminauth.ClassIP, ip) {
			return
		}
		if n.login && !s.allow(w, r, adminauth.ClassLogin, ip) {
			return
		}
		if n.db && s.admins == nil {
			s.fail(w, r, http.StatusServiceUnavailable, "persistence required")
			return
		}
		// Browser requests (no Authorization header: a session cookie, or the public login) on an
		// unsafe method pass http.CrossOriginProtection BEFORE the credential is resolved: the
		// login is covered (no login CSRF), and a refused request never reaches admin_session_check,
		// so it cannot slide the session's idle time. Bearer callers are not browsers.
		if r.Header.Get("Authorization") == "" && !safeMethod(r.Method) {
			if err := s.cop.Check(r); err != nil {
				s.fail(w, r, http.StatusForbidden, "cross-origin request refused")
				return
			}
		}
		if n.public {
			h(w, r)
			return
		}
		who, status := s.resolve(r, n.project)
		if status != 0 {
			if status == http.StatusInternalServerError {
				s.fail(w, r, status, "internal error")
			} else {
				s.fail(w, r, status, "unauthorized")
			}
			return
		}
		if (n.keyOnly && !who.key) || (n.cookieOnly && !who.cookie) {
			s.fail(w, r, http.StatusUnauthorized, "unauthorized")
			return
		}
		if who.admin && who.role < n.role { // a project key reaches only project routes (resolve)
			s.fail(w, r, http.StatusForbidden, "forbidden")
			return
		}
		if who.mustChange && !n.mustChangeOK {
			s.fail(w, r, http.StatusForbidden, "password change required")
			return
		}
		if who.cookie && !s.allow(w, r, adminauth.ClassUser, who.user.String()) {
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), callerKey{}, who)))
	})
}

func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// allow takes a token from one bucket; a refusal answers 429 with Retry-After in whole seconds.
func (s *Server) allow(w http.ResponseWriter, r *http.Request, c adminauth.Class, key string) bool {
	if s.limiter == nil {
		return true
	}
	ok, retry := s.limiter.Allow(c, key)
	if ok {
		return true
	}
	w.Header().Set("Retry-After", strconv.Itoa(max(1, int(math.Ceil(retry.Seconds())))))
	s.fail(w, r, http.StatusTooManyRequests, "rate limited")
	return false
}

// resolve turns the request's credential into a caller. An Authorization header wins (the admin
// key, or a project key when allowProject); otherwise the session cookie. status 0 = resolved,
// 401 = no valid credential, 500 = the session store failed.
func (s *Server) resolve(r *http.Request, allowProject bool) (caller, int) {
	if r.Header.Get("Authorization") != "" {
		if who, ok := s.bearer(r, allowProject); ok {
			return who, 0
		}
		return caller{}, http.StatusUnauthorized
	}
	ck, err := r.Cookie(CookieName)
	if err != nil || ck.Value == "" || s.admins == nil {
		return caller{}, http.StatusUnauthorized
	}
	hash := adminauth.HashToken(ck.Value)
	u, err := s.admins.SessionCheck(r.Context(), hash)
	switch {
	case errors.Is(err, ErrNotFound):
		return caller{}, http.StatusUnauthorized
	case err != nil:
		logger.L.Error("API: session check failed", "err", err)
		return caller{}, http.StatusInternalServerError
	}
	return caller{
		admin: true, actor: "user:" + u.ID.String(), role: u.Role, cookie: true, user: u.ID,
		mustChange: u.MustChangePassword, token: hash,
	}, 0
}

// setSessionCookie sets (maxAge > 0) or clears (maxAge < 0) the session cookie (D2).
func (s *Server) setSessionCookie(w http.ResponseWriter, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name: CookieName, Value: value, Path: "/", MaxAge: maxAge,
		HttpOnly: true, Secure: s.secureCookie, SameSite: http.SameSiteStrictMode,
	})
}

// portalAudit records a portal event (no project).
func (s *Server) portalAudit(actor, action, subject string, detail map[string]string) {
	s.auditor.Audit(tenant.Event{Actor: actor, Action: action, Subject: subject, Detail: detail})
}

// validEmail is a light shape check: the address is an identifier here, never mailed. A NUL byte
// is refused here because Postgres text cannot hold one (it would surface as a 500).
func validEmail(e string) bool {
	at := strings.IndexByte(e, '@')
	return len(e) >= 3 && len(e) <= maxEmailLen && at > 0 && at < len(e)-1 && !strings.ContainsAny(e, " \t\r\n\x00")
}

// validName bounds a display name; no NUL (see validEmail).
func validName(n string) bool { return len(n) <= maxNameLen && !strings.ContainsRune(n, 0) }

// bootstrap answers POST /admin/bootstrap: the first super_admin, once (criterion 1). The latch
// row makes it race-safe in SQL (admin_bootstrap()); the early BootstrapDone check only saves a
// bcrypt when it is already done.
func (s *Server) bootstrap(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	done, err := s.admins.BootstrapDone(ctx)
	if err != nil {
		s.internalError(w, r, "bootstrap check", err)
		return
	}
	if done {
		s.fail(w, r, http.StatusGone, "bootstrap already done")
		return
	}
	var req BootstrapRequest
	if !decode(w, r, &req) {
		return
	}
	if !validEmail(req.Email) || !validName(req.Name) {
		s.fail(w, r, http.StatusBadRequest, "invalid email or name")
		return
	}
	if err := adminauth.CheckPassword(req.Password); err != nil {
		s.fail(w, r, http.StatusBadRequest, err.Error())
		return
	}
	hash, err := adminauth.HashPassword(req.Password, s.bcryptCost)
	if err != nil {
		s.internalError(w, r, "password hash", err)
		return
	}
	id, err := s.admins.Bootstrap(ctx, req.Email, req.Name, hash)
	switch {
	case err != nil:
		s.internalError(w, r, "bootstrap", err)
		return
	case id == uuid.Nil:
		// admin_bootstrap() returns NULL on any unique violation: the latch (done → 410) or an email
		// a bearer-created user already holds (latch still open → 409).
		if done, err := s.admins.BootstrapDone(ctx); err == nil && !done {
			s.fail(w, r, http.StatusConflict, "email taken")
			return
		}
		s.fail(w, r, http.StatusGone, "bootstrap already done")
		return
	}
	s.portalAudit("admin", "admin_bootstrap", id.String(), map[string]string{"user_id": id.String(), "role": "super_admin"})
	logger.L.Info("API: admin bootstrap done", "user", id.String())
	writeJSON(w, http.StatusCreated, s.meFor(ctx, caller{actor: "user:" + id.String(), role: adminauth.RoleSuperAdmin, cookie: true, user: id}))
}

// login answers POST /admin/auth/login (criteria 2–4).
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req LoginRequest
	if !decode(w, r, &req) {
		return
	}
	ip := adminauth.ClientIP(r, s.trustProxy)
	dummy := s.dummy()
	var u LoginUser
	err := ErrNotFound // an address no account can have reads as unknown (no 500 on a NUL byte)
	if validEmail(req.Email) {
		u, err = s.admins.UserForLogin(ctx, req.Email)
	}
	switch {
	case errors.Is(err, ErrNotFound):
		if dummy != "" {
			adminauth.PasswordMatches(dummy, req.Password) // the same bcrypt cost as a real user
		}
		s.signInFailed(w, r, "unknown", ip)
		return
	case err != nil:
		s.internalError(w, r, "user for login", err)
		return
	}
	match := adminauth.PasswordMatches(u.PasswordHash, req.Password)
	locked := u.LockedUntil.After(time.Now())
	if !match || locked || u.Disabled || u.Role == adminauth.RoleNone {
		if !match && !locked && !u.Disabled { // only an active, unlocked user's wrong password counts
			if until, err := s.admins.LoginRecord(ctx, u.ID, false); err != nil {
				logger.L.Error("API: login record failed", "user", u.ID.String(), "err", err)
			} else if until.After(time.Now()) {
				s.portalAudit("user:"+u.ID.String(), "admin_locked", u.ID.String(), map[string]string{"until": fmtTime(until)})
			}
		}
		s.signInFailed(w, r, u.ID.String(), ip)
		return
	}
	if _, err := s.admins.LoginRecord(ctx, u.ID, true); err != nil {
		s.internalError(w, r, "login record", err, "user", u.ID.String())
		return
	}
	raw, hash, err := adminauth.NewToken()
	if err != nil {
		s.internalError(w, r, "session token", err)
		return
	}
	expires, err := s.admins.SessionOpen(ctx, hash, u.ID, ip, r.UserAgent())
	if err != nil {
		s.internalError(w, r, "session open", err, "user", u.ID.String())
		return
	}
	s.setSessionCookie(w, raw, int(adminauth.SessionTTL/time.Second))
	s.portalAudit("user:"+u.ID.String(), "admin_login", u.ID.String(), map[string]string{"ip": ip})
	logger.L.Info("API: admin signed in", "user", u.ID.String())
	out := s.meFor(ctx, caller{actor: "user:" + u.ID.String(), role: u.Role, cookie: true, user: u.ID})
	out.ExpiresAt = fmtTime(expires)
	writeJSON(w, http.StatusOK, out)
}

// signInFailed writes the audit row and the one failure reply (subject: the user id, or
// "unknown" for an email with no account; the email itself is never recorded).
func (s *Server) signInFailed(w http.ResponseWriter, r *http.Request, subject, ip string) {
	s.portalAudit("anonymous", "admin_login_failed", subject, map[string]string{"ip": ip})
	s.fail(w, r, http.StatusUnauthorized, errSignIn)
}

// logout answers POST /admin/auth/logout: the session row is deleted (instant revoke) and the
// cookie cleared.
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	who, _ := s.authenticate(r, false)
	if err := s.admins.SessionDelete(r.Context(), who.token); err != nil {
		s.internalError(w, r, "session delete", err, "user", who.user.String())
		return
	}
	s.setSessionCookie(w, "", -1)
	s.portalAudit(who.actor, "admin_logout", who.user.String(), nil)
	w.WriteHeader(http.StatusNoContent)
}

// me answers GET /admin/auth/me.
func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	who, _ := s.authenticate(r, false)
	writeJSON(w, http.StatusOK, s.meFor(r.Context(), who))
}

func (s *Server) meFor(ctx context.Context, who caller) MeResponse {
	out := MeResponse{Actor: who.actor, Role: who.role.String(), MustChangePassword: who.mustChange}
	if who.cookie {
		if u, err := s.admins.User(ctx, who.user); err == nil {
			info := userInfo(u)
			out.User, out.MustChangePassword = &info, u.MustChangePassword
		} else {
			logger.L.Warn("API: me lookup failed", "user", who.user.String(), "err", err)
		}
	}
	return out
}

// changePassword answers POST /admin/auth/password: the current password is required; every other
// session of the user ends; must_change_password clears (criterion 6's way out).
func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	who, _ := s.authenticate(r, false)
	var req PasswordChangeRequest
	if !decode(w, r, &req) {
		return
	}
	cur, err := s.admins.PasswordHash(r.Context(), who.user)
	if err != nil {
		s.internalError(w, r, "password hash", err, "user", who.user.String())
		return
	}
	if !adminauth.PasswordMatches(cur, req.CurrentPassword) {
		s.fail(w, r, http.StatusForbidden, "current password does not match")
		return
	}
	if err := adminauth.CheckPassword(req.NewPassword); err != nil {
		s.fail(w, r, http.StatusBadRequest, err.Error())
		return
	}
	if req.NewPassword == req.CurrentPassword {
		s.fail(w, r, http.StatusBadRequest, "new password must differ")
		return
	}
	hash, err := adminauth.HashPassword(req.NewPassword, s.bcryptCost)
	if err != nil {
		s.internalError(w, r, "password hash", err)
		return
	}
	if err := s.admins.SetPassword(r.Context(), who.user, hash, false, who.token); err != nil {
		s.internalError(w, r, "set password", err, "user", who.user.String())
		return
	}
	s.portalAudit(who.actor, "admin_password_changed", who.user.String(), nil)
	w.WriteHeader(http.StatusNoContent)
}

func userInfo(u AdminUser) UserInfo {
	out := UserInfo{
		UserID: u.ID.String(), Email: u.Email, Name: u.Name, Role: u.Role.String(),
		MustChangePassword: u.MustChangePassword, DisabledAt: fmtTime(u.DisabledAt),
		CreatedAt: fmtTime(u.CreatedAt), PasswordChangedAt: fmtTime(u.PasswordChangedAt), LastLoginAt: fmtTime(u.LastLoginAt),
	}
	if u.LockedUntil.After(time.Now()) {
		out.LockedUntil = fmtTime(u.LockedUntil)
	}
	if u.CreatedBy != uuid.Nil {
		out.CreatedBy = u.CreatedBy.String()
	}
	return out
}
