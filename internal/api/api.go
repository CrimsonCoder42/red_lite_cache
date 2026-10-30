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
	"time"

	"github.com/google/uuid"

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

// Server serves /admin/*. It never logs a key, a password or a value: key_prefix only.
type Server struct {
	reg      *sheet.Registry
	store    Store
	adminKey []byte // empty = every admin-key route answers 401
	now      func() time.Time
	leader   func() bool                                // writes are refused (503 not leader) on a replica: its registry is a mirror
	onRevoke []func(project uuid.UUID, sheets []string) // revoke.go OnRevoke

	tenants   *tenant.Directory
	sessions  *session.Registry
	auditor   tenant.Auditor
	propagate func(protocol.ReplicaControl)
	peer      *peer // nil = no peer to list or forward to
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
		reg: reg, store: store, adminKey: []byte(adminKey), now: time.Now, leader: leader,
		tenants: opts.Tenants, sessions: opts.Sessions, auditor: opts.Auditor, propagate: opts.Propagate,
	}
	if opts.PeerURL != "" {
		s.peer = newPeer(opts.PeerURL, adminKey)
	}
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

// Register mounts the routes on mux (stdlib method patterns, Go 1.22+).
func (s *Server) Register(mux interface {
	Handle(pattern string, h http.Handler)
}) {
	mux.Handle("POST /admin/projects", http.HandlerFunc(s.createProject))
	mux.Handle("POST /admin/projects/{id}/sheets", http.HandlerFunc(s.createSheet))
	mux.Handle("GET /admin/sheets/{id}/export", http.HandlerFunc(s.exportSheet))
	mux.Handle("POST /admin/projects/{id}/revoke", http.HandlerFunc(s.revokeProject)) // revoke.go (D-021)
	// tenancy.go (D-027, D-028, D-029)
	mux.Handle("GET /admin/projects", http.HandlerFunc(s.listProjects))
	mux.Handle("GET /admin/projects/{id}", http.HandlerFunc(s.getProject))
	mux.Handle("POST /admin/projects/{id}/keys", http.HandlerFunc(s.issueKey))
	mux.Handle("DELETE /admin/keys/{id}", http.HandlerFunc(s.revokeKey))
	mux.Handle("POST /admin/keys/{id}/rotate", http.HandlerFunc(s.rotateKey))
	mux.Handle("POST /admin/projects/{id}/suspend", http.HandlerFunc(s.suspendProject))
	mux.Handle("POST /admin/projects/{id}/resume", http.HandlerFunc(s.resumeProject))
	// sessions.go (D-026, D-030)
	mux.Handle("GET /admin/sessions", http.HandlerFunc(s.listSessions))
	mux.Handle("GET /admin/sheets/{name}/sessions", http.HandlerFunc(s.listSheetSessions))
	mux.Handle("DELETE /admin/sessions/{id}", http.HandlerFunc(s.killSession))
}

// caller is who presented the bearer key.
type caller struct {
	admin   bool
	project uuid.UUID // set for a project key
	actor   string    // audit actor: "admin" or "key:<prefix>"
}

// authenticate resolves the bearer key: the admin key, or a project API key when allowProject.
func (s *Server) authenticate(r *http.Request, allowProject bool) (caller, bool) {
	key, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || key == "" {
		return caller{}, false
	}
	if len(s.adminKey) > 0 && subtle.ConstantTimeCompare([]byte(key), s.adminKey) == 1 {
		return caller{admin: true, actor: "admin"}, true
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
	s.audit(id, "project_created", id.String(), map[string]string{"key_id": key.ID.String(), "key_prefix": key.Prefix})
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
