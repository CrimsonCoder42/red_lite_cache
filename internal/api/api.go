package api

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/CrimsonCoder42/red_lite_cache/internal/adminauth"
	"github.com/CrimsonCoder42/red_lite_cache/internal/logger"
	"github.com/CrimsonCoder42/red_lite_cache/internal/session"
	"github.com/CrimsonCoder42/red_lite_cache/internal/sheet"
	"github.com/CrimsonCoder42/red_lite_cache/internal/tenant"
	"github.com/CrimsonCoder42/red_lite_cache/protocol"
)

// keyPrefixLen is how much of an API key is kept in clear for logs and listing.
const keyPrefixLen = 8

// maxBody bounds every request body.
const maxBody = 64 << 10

// Server serves /admin/*. It never logs a key, a password, a cookie, a hash or a value: key_prefix
// and ids only.
type Server struct {
	reg          *sheet.Registry
	store        Store
	adminKey     []byte   // empty = every admin-key route answers 401
	adminKeyHash [32]byte // sha256(adminKey): compared, so the length of the key does not leak (S11)
	now          func() time.Time
	leader       func() bool                                // writes are refused (503 not leader) on a replica: its registry is a mirror
	onRevoke     []func(project uuid.UUID, sheets []string) // revoke.go OnRevoke

	tenants   *tenant.Directory
	sessions  *session.Registry
	auditor   tenant.Auditor
	propagate func(protocol.ReplicaControl)
	peer      *peer // nil = no peer to list or forward to

	// ── admin portal (S11, admin_auth.go) ── set once by New, read-only afterwards.
	admins       AdminStore                  // nil = persistence off: the DB routes answer 503
	limiter      *adminauth.Limiter          // nil = no rate limits (unit tests); the app owns its Run
	trustProxy   bool                        // D6: REDLITE_ADMIN_TRUSTED_PROXY
	secureCookie bool                        // false only in development (D2)
	bcryptCost   int                         // adminauth.BcryptCost unless a test lowers it
	dummyOnce    sync.Once                   // guards dummyHash (admin_auth.go dummy)
	dummyHash    string                      // compared for an unknown email so it costs one bcrypt (criterion 2)
	cop          *http.CrossOriginProtection // unsafe methods of cookie callers (criterion 7)
	stats        func() NodeStats            // this node's GET /admin/stats entry; nil = role and counts omitted
	startedAt    time.Time                   // NodeStats.StartedAt (uptime)
	// ── end admin portal ──
}

// Options are the tenancy dependencies of the API (D-026…D-030). Zero fields get a local default:
// a password-mode Directory over the store, an empty session registry, NopAuditor, no propagation,
// no peer.
type Options struct {
	Tenants  *tenant.Directory
	Sessions *session.Registry
	Auditor  tenant.Auditor
	// Propagate carries a suspend/resume/key revoke/session kill to the other instances (NOTIFY
	// redlite_control with persistence, the leader → replica stream without). Must not block long.
	Propagate func(protocol.ReplicaControl)
	// PeerURL is the other node's http(s)://host:port; GET /admin/sessions merges its sessions and
	// DELETE /admin/sessions/{id} forwards there when the session is not on this node. "" = none.
	PeerURL string

	// ── admin portal (S11) ──
	// Admin is the portal store; nil (persistence off) makes bootstrap, auth/*, users and audit
	// answer 503 "persistence required". Every bearer route is unaffected.
	Admin AdminStore
	// Limiter rate-limits every /admin route (adminauth.DefaultLimits in the app); nil = none.
	// Its Run belongs to the caller.
	Limiter *adminauth.Limiter
	// TrustedProxy is D6 (REDLITE_ADMIN_TRUSTED_PROXY): the last X-Forwarded-For hop is the client
	// when RemoteAddr is loopback.
	TrustedProxy bool
	// SecureCookie sets Secure on the session cookie (every environment except development).
	SecureCookie bool
	// BcryptCost is adminauth.BcryptCost when 0; tests pass bcrypt.MinCost.
	BcryptCost int
	// Stats is this node's GET /admin/stats entry (role, epoch, instance, counts); nil = the entry
	// carries only the node name and started_at.
	Stats func() NodeStats
	// ── end admin portal ──
}

// New builds the API over the process registry and a project store. leader reports whether this
// node is the leader; nil means always (a standalone test server).
func New(reg *sheet.Registry, store Store, adminKey string, leader func() bool, opts Options) *Server {
	if leader == nil {
		leader = func() bool { return true }
	}
	if opts.Tenants == nil {
		opts.Tenants = tenant.New(store, false)
	}
	if opts.Sessions == nil {
		opts.Sessions = session.NewRegistry("")
	}
	if opts.Auditor == nil {
		opts.Auditor = tenant.NopAuditor{}
	}
	if opts.Propagate == nil {
		opts.Propagate = func(protocol.ReplicaControl) {}
	}
	s := &Server{
		reg: reg, store: store, adminKey: []byte(adminKey), adminKeyHash: sha256.Sum256([]byte(adminKey)), now: time.Now, leader: leader,
		tenants: opts.Tenants, sessions: opts.Sessions, auditor: opts.Auditor, propagate: opts.Propagate,
	}
	if opts.PeerURL != "" {
		s.peer = newPeer(opts.PeerURL, adminKey)
	}
	s.initPortal(opts)
	return s
}

// requireLeader answers 503 not leader on a replica: a project or sheet created there would live
// only in the mirror and be removed by the leader's next snapshot.
func (s *Server) requireLeader(w http.ResponseWriter, r *http.Request) bool {
	if s.leader() {
		return true
	}
	s.fail(w, r, http.StatusServiceUnavailable, protocol.ErrMsgNotLeader)
	return false
}

// Register mounts the routes on mux (stdlib method patterns, Go 1.22+). Every route goes through
// guard (admin_auth.go): rate limits, credential, role (the spec's route table), then the handler.
func (s *Server) Register(mux interface {
	Handle(pattern string, h http.Handler)
}) {
	viewer := need{role: adminauth.RoleViewer}
	operator := need{role: adminauth.RoleAdmin}
	route := func(pattern string, n need, h http.HandlerFunc) { mux.Handle(pattern, s.guard(n, h)) }

	route("POST /admin/projects", operator, s.createProject)
	route("POST /admin/projects/{id}/sheets", need{role: adminauth.RoleAdmin, project: true}, s.createSheet)
	route("GET /admin/sheets/{id}/export", need{role: adminauth.RoleSuperAdmin, project: true}, s.exportSheet) // D8
	route("POST /admin/projects/{id}/revoke", operator, s.revokeProject)                                       // revoke.go (D-021)
	// tenancy.go (D-027, D-028, D-029)
	route("GET /admin/projects", viewer, s.listProjects)
	route("GET /admin/projects/{id}", viewer, s.getProject)
	route("POST /admin/projects/{id}/keys", operator, s.issueKey)
	route("DELETE /admin/keys/{id}", operator, s.revokeKey)
	route("POST /admin/keys/{id}/rotate", operator, s.rotateKey)
	route("POST /admin/projects/{id}/suspend", operator, s.suspendProject)
	route("POST /admin/projects/{id}/resume", operator, s.resumeProject)
	// sessions.go (D-026, D-030)
	route("GET /admin/sessions", viewer, s.listSessions)
	route("GET /admin/sheets/{name}/sessions", viewer, s.listSheetSessions)
	route("DELETE /admin/sessions/{id}", operator, s.killSession)
	// admin portal (S11): admin_auth.go, admin_users.go, admin_reads.go
	route("POST /admin/bootstrap", need{keyOnly: true, db: true, login: true}, s.bootstrap)
	route("POST /admin/auth/login", need{public: true, db: true, login: true}, s.login)
	route("POST /admin/auth/logout", need{role: adminauth.RoleViewer, db: true, cookieOnly: true, mustChangeOK: true}, s.logout)
	route("GET /admin/auth/me", need{role: adminauth.RoleViewer, db: true, mustChangeOK: true}, s.me)
	route("POST /admin/auth/password", need{role: adminauth.RoleViewer, db: true, cookieOnly: true, login: true, mustChangeOK: true}, s.changePassword)
	route("GET /admin/users", need{role: adminauth.RoleSuperAdmin, db: true}, s.listUsers)
	route("POST /admin/users", need{role: adminauth.RoleSuperAdmin, db: true}, s.createUser)
	route("PATCH /admin/users/{id}", need{role: adminauth.RoleSuperAdmin, db: true}, s.patchUser)
	route("POST /admin/users/{id}/reset-password", need{role: adminauth.RoleSuperAdmin, db: true}, s.resetPassword)
	route("GET /admin/stats", viewer, s.statsHandler)
	route("GET /admin/audit", need{role: adminauth.RoleViewer, db: true}, s.auditHandler)
}

// caller is who made the request: the bearer admin key, a project API key, or a signed-in admin
// user (session cookie, S11).
type caller struct {
	admin   bool      // an operator (admin key or signed-in user); its role decides what it may do
	project uuid.UUID // set for a project key
	actor   string    // audit actor: "admin", "key:<prefix>" or "user:<uuid>"

	role       adminauth.Role // RoleSuperAdmin for the admin key; RoleNone for a project key
	key        bool           // the bearer admin key
	cookie     bool           // a session cookie
	user       uuid.UUID      // the signed-in user (cookie)
	mustChange bool           // the user must change the password before anything else
	token      []byte         // sha256 of the session cookie (logout, password change)
}

// authenticate is the caller guard resolved for this request; allowProject admits a project key.
// Without a guard in front (no route is mounted without one) it resolves the bearer key itself.
func (s *Server) authenticate(r *http.Request, allowProject bool) (caller, bool) {
	if c, ok := r.Context().Value(callerKey{}).(caller); ok {
		if !c.admin && !allowProject { // a project key (the 'default' project's id is uuid.Nil)
			return caller{}, false
		}
		return c, true
	}
	return s.bearer(r, allowProject)
}

// adminKeyCaller reports whether the request carries the bearer admin key (no I/O).
func (s *Server) adminKeyCaller(r *http.Request) (caller, bool) {
	key, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || key == "" || len(s.adminKey) == 0 {
		return caller{}, false
	}
	got := sha256.Sum256([]byte(key))
	if subtle.ConstantTimeCompare(got[:], s.adminKeyHash[:]) != 1 {
		return caller{}, false
	}
	return caller{admin: true, actor: "admin", role: adminauth.RoleSuperAdmin, key: true}, true
}

// bearer resolves the Authorization header: the admin key, or a project API key when allowProject.
// The admin key is compared as sha256 of both sides so a wrong length takes the same time.
func (s *Server) bearer(r *http.Request, allowProject bool) (caller, bool) {
	key, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || key == "" {
		return caller{}, false
	}
	if c, ok := s.adminKeyCaller(r); ok {
		return c, true
	}
	if !allowProject {
		return caller{}, false
	}
	project, err := s.store.ProjectByKeyHash(r.Context(), hashKey(key))
	if err != nil {
		if !errors.Is(err, ErrNotFound) {
			logger.L.Error("API: key lookup failed", "err", err)
		}
		return caller{}, false
	}
	return caller{project: project, actor: "key:" + key[:min(len(key), keyPrefixLen)]}, true
}

func (s *Server) createProject(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authenticate(r, false); !ok {
		s.fail(w, r, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !s.requireLeader(w, r) {
		return
	}
	var req protocol.CreateProjectRequest
	if !decode(w, r, &req) {
		return
	}
	if req.Name == "" {
		s.fail(w, r, http.StatusBadRequest, "name required")
		return
	}
	key, raw, err := newKey()
	if err != nil {
		logger.L.Error("API: key generation failed", "err", err)
		s.fail(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	id := uuid.New()
	key.ProjectID, key.Label = id, req.Label
	if err := s.store.CreateProject(r.Context(), id, req.Name, key); err != nil {
		if errors.Is(err, ErrConflict) {
			s.fail(w, r, http.StatusConflict, "project name taken")
			return
		}
		logger.L.Error("API: create project failed", "project_name", req.Name, "err", err)
		s.fail(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	s.audit(r, id, "project_created", id.String(), map[string]string{"key_id": key.ID.String(), "key_prefix": key.Prefix})
	logger.L.Info("API: project created", "project", id.String(), "project_name", req.Name, "key_prefix", key.Prefix)
	writeJSON(w, http.StatusCreated, protocol.CreateProjectResponse{
		ProjectID: id.String(), Name: req.Name, APIKey: raw, KeyPrefix: key.Prefix,
	})
}

func (s *Server) createSheet(w http.ResponseWriter, r *http.Request) {
	who, ok := s.authenticate(r, true)
	if !ok {
		s.fail(w, r, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !s.requireLeader(w, r) {
		return
	}
	project, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		s.fail(w, r, http.StatusBadRequest, "invalid project id")
		return
	}
	if !who.admin && who.project != project {
		s.fail(w, r, http.StatusNotFound, "project not found") // a project never learns another exists
		return
	}
	exists, err := s.store.ProjectExists(r.Context(), project)
	if err != nil {
		logger.L.Error("API: project lookup failed", "project", project.String(), "err", err)
		s.fail(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	if !exists {
		s.fail(w, r, http.StatusNotFound, "project not found")
		return
	}
	var req protocol.CreateSheetRequest
	if !decode(w, r, &req) {
		return
	}
	if _, err := s.reg.Create(project, req.Name, req.Password); err != nil {
		switch {
		case errors.Is(err, sheet.ErrNameRequired):
			s.fail(w, r, http.StatusBadRequest, "name and password required")
		case errors.Is(err, sheet.ErrSheetExists):
			s.fail(w, r, http.StatusConflict, "sheet name taken")
		default:
			logger.L.Error("API: create sheet failed", "sheet", req.Name, "err", err)
			s.fail(w, r, http.StatusInternalServerError, "internal error")
		}
		return
	}
	s.auditAs(who.actor, project, "sheet_created", req.Name, map[string]string{"sheet": req.Name})
	logger.L.Info("API: sheet created", "sheet", req.Name, "project", project.String())
	writeJSON(w, http.StatusCreated, protocol.CreateSheetResponse{Sheet: req.Name, ProjectID: project.String()})
}

func (s *Server) exportSheet(w http.ResponseWriter, r *http.Request) {
	who, ok := s.authenticate(r, true)
	if !ok {
		s.fail(w, r, http.StatusUnauthorized, "unauthorized")
		return
	}
	sh, ok := s.reg.Get(r.PathValue("id"))
	if !ok || (!who.admin && sh.ProjectID() != who.project) {
		s.fail(w, r, http.StatusNotFound, "sheet not found")
		return
	}
	now := s.now()
	var out protocol.SheetExport
	sh.Snapshot(func(snap sheet.Snapshot) {
		out = protocol.SheetExport{
			Sheet: snap.Name, ProjectID: snap.ProjectID.String(), Seq: snap.Seq,
			Count: len(snap.Entries), Entries: make(map[string]protocol.ExportEntry, len(snap.Entries)),
		}
		for k, e := range snap.Entries {
			out.Entries[k] = protocol.ExportEntry{Value: e.Value, ExpiresInMS: e.Remaining(now).Milliseconds()}
		}
	})
	logger.L.Info("API: sheet exported", "sheet", out.Sheet, "count", out.Count)
	writeJSON(w, http.StatusOK, out)
}

// fail answers status with the error body and logs a handled failure at Warn (once, here).
func (s *Server) fail(w http.ResponseWriter, r *http.Request, status int, msg string) {
	logger.L.Warn("API: request refused", "method", r.Method, "path", r.URL.Path, "status", status, "reason", msg)
	writeJSON(w, status, protocol.APIError{Error: msg})
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, protocol.APIError{Error: "invalid JSON"})
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// newKey returns a fresh API key ("rlk_" + 32 random bytes, base64url) and its stored form.
func newKey() (Key, string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return Key{}, "", err
	}
	raw := "rlk_" + base64.RawURLEncoding.EncodeToString(b)
	return Key{ID: uuid.New(), Hash: hashKey(raw), Prefix: raw[:keyPrefixLen]}, raw, nil
}

// hashKey is the at-rest form of an API key: sha256.
func hashKey(key string) []byte {
	sum := sha256.Sum256([]byte(key))
	return sum[:]
}
