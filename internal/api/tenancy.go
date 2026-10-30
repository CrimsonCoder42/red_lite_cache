package api

// Tenancy routes (D-027 suspend, D-028 keys, D-029 listing): admin key only. Writes are leader-only
// (503 not leader on a replica); listings answer on any node. Every mutation writes one audit row
// and, where other nodes hold state, is propagated (Options.Propagate).

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/CrimsonCoder42/red_lite_cache/internal/logger"
	"github.com/CrimsonCoder42/red_lite_cache/internal/session"
	"github.com/CrimsonCoder42/red_lite_cache/internal/tenant"
	"github.com/CrimsonCoder42/red_lite_cache/protocol"
)

// Paging (D-029): limit default 50, max 200; the cursor is opaque (base64url of created_at|id).
const (
	defaultPageLimit = 50
	maxPageLimit     = 200
	maxLabelLen      = 200
)

// adminOnly answers 401 unless the caller presents the admin key.
func (s *Server) adminOnly(w http.ResponseWriter, r *http.Request) bool {
	if _, ok := s.authenticate(r, false); !ok {
		s.fail(w, r, http.StatusUnauthorized, "unauthorized")
		return false
	}
	return true
}

// pathID parses the {name} path value as a uuid, answering 400 "invalid <what> id" when it is not.
func (s *Server) pathID(w http.ResponseWriter, r *http.Request, name, what string) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue(name))
	if err != nil {
		s.fail(w, r, http.StatusBadRequest, "invalid "+what+" id")
		return uuid.Nil, false
	}
	return id, true
}

// audit records an admin-key mutation; auditAs one made with a project key.
func (s *Server) audit(project uuid.UUID, action, subject string, detail map[string]string) {
	s.auditAs("admin", project, action, subject, detail)
}

func (s *Server) auditAs(actor string, project uuid.UUID, action, subject string, detail map[string]string) {
	s.auditor.Audit(tenant.Event{Project: project, Actor: actor, Action: action, Subject: subject, Detail: detail})
}

// cursor is a keyset position (created_at, id).
type cursor struct {
	at time.Time
	id string
}

func (c cursor) encode() string {
	return base64.RawURLEncoding.EncodeToString([]byte(c.at.UTC().Format(time.RFC3339Nano) + "|" + c.id))
}

// after reports whether (at, id) sorts after c.
func (c cursor) after(at time.Time, id string) bool {
	if k := at.Compare(c.at); k != 0 {
		return k > 0
	}
	return id > c.id
}

// pageParams reads ?limit= and ?cursor=. A bad value answers 400; a limit above the max is clamped.
func (s *Server) pageParams(w http.ResponseWriter, r *http.Request) (int, cursor, bool) {
	q := r.URL.Query()
	limit := defaultPageLimit
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			s.fail(w, r, http.StatusBadRequest, "invalid limit")
			return 0, cursor{}, false
		}
		limit = min(n, maxPageLimit)
	}
	var c cursor
	if v := q.Get("cursor"); v != "" {
		b, err := base64.RawURLEncoding.DecodeString(v)
		at, id, ok := strings.Cut(string(b), "|")
		var t time.Time
		if err == nil && ok {
			t, err = time.Parse(time.RFC3339Nano, at)
		}
		if err != nil || !ok || id == "" {
			s.fail(w, r, http.StatusBadRequest, "invalid cursor")
			return 0, cursor{}, false
		}
		c = cursor{at: t, id: id}
	}
	return limit, c, true
}

// fmtTime is a time as RFC 3339 UTC (nanoseconds kept), "" for the zero time.
func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// decodeOptional decodes a JSON body into v; an empty body leaves v as is.
func decodeOptional(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, protocol.APIError{Error: "invalid JSON"})
		return false
	}
	return true
}

// internalError logs an unhandled store failure once and answers 500.
func (s *Server) internalError(w http.ResponseWriter, r *http.Request, what string, err error, attrs ...any) {
	logger.L.Error("API: "+what+" failed", append(attrs, "err", err)...)
	s.fail(w, r, http.StatusInternalServerError, "internal error")
}

// sheetsByProject counts the registry's sheets per project (the registry is the live copy on
// both stores; a DB sheet row may lag the writer).
func (s *Server) sheetsByProject() map[uuid.UUID]int {
	out := map[uuid.UUID]int{}
	for _, sh := range s.reg.Sheets() {
		out[sh.ProjectID()]++
	}
	return out
}

func summary(p Project, sheets int) protocol.ProjectSummary {
	return protocol.ProjectSummary{
		ProjectID: p.ID.String(), Name: p.Name, CreatedAt: fmtTime(p.CreatedAt),
		SuspendedAt: fmtTime(p.SuspendedAt), Keys: p.Keys, Sheets: sheets,
	}
}

// listProjects answers GET /admin/projects (D-029): one keyset page of ProjectSummary.
func (s *Server) listProjects(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) {
		return
	}
	limit, c, ok := s.pageParams(w, r)
	if !ok {
		return
	}
	var afterID uuid.UUID
	if c.id != "" {
		id, err := uuid.Parse(c.id)
		if err != nil {
			s.fail(w, r, http.StatusBadRequest, "invalid cursor")
			return
		}
		afterID = id
	}
	rows, err := s.store.ListProjects(r.Context(), c.at, afterID, limit+1)
	if err != nil {
		s.internalError(w, r, "list projects", err)
		return
	}
	counts := s.sheetsByProject()
	page := protocol.Page[protocol.ProjectSummary]{Items: make([]protocol.ProjectSummary, 0, min(len(rows), limit))}
	for i, p := range rows {
		if i == limit {
			last := rows[limit-1]
			page.NextCursor = cursor{at: last.CreatedAt, id: last.ID.String()}.encode()
			break
		}
		page.Items = append(page.Items, summary(p, counts[p.ID]))
	}
	writeJSON(w, http.StatusOK, page)
}

// getProject answers GET /admin/projects/{id} (D-029): keys (never a hash) and sheets with their
// live entry counts and this node's collaborator (session) counts.
func (s *Server) getProject(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) {
		return
	}
	id, ok := s.pathID(w, r, "id", "project")
	if !ok {
		return
	}
	p, keys, err := s.store.GetProject(r.Context(), id)
	switch {
	case errors.Is(err, ErrNotFound):
		s.fail(w, r, http.StatusNotFound, "project not found")
		return
	case err != nil:
		s.internalError(w, r, "get project", err, "project", id.String())
		return
	}
	out := protocol.ProjectDetail{APIKeys: make([]protocol.KeyInfo, len(keys)), SheetsList: []protocol.SheetInfo{}}
	for i, k := range keys {
		out.APIKeys[i] = keyInfo(k)
	}
	for _, sh := range s.reg.Sheets() {
		if sh.ProjectID() == id {
			out.SheetsList = append(out.SheetsList, protocol.SheetInfo{
				Name: sh.Name(), Keys: sh.Len(), Collaborators: len(s.sessions.List(session.Filter{Sheet: sh.Name()})),
			})
		}
	}
	slices.SortFunc(out.SheetsList, func(a, b protocol.SheetInfo) int { return strings.Compare(a.Name, b.Name) })
	out.ProjectSummary = summary(p, len(out.SheetsList))
	writeJSON(w, http.StatusOK, out)
}

func keyInfo(k Key) protocol.KeyInfo {
	return protocol.KeyInfo{
		KeyID: k.ID.String(), KeyPrefix: k.Prefix, Label: k.Label, CreatedAt: fmtTime(k.CreatedAt),
		RevokedAt: fmtTime(k.RevokedAt), LastUsedAt: fmtTime(k.LastUsedAt),
	}
}

// keyRequest reads the optional IssueKeyRequest body and makes a fresh key carrying its label.
func (s *Server) keyRequest(w http.ResponseWriter, r *http.Request) (Key, string, bool) {
	var req protocol.IssueKeyRequest
	if !decodeOptional(w, r, &req) {
		return Key{}, "", false
	}
	if len(req.Label) > maxLabelLen {
		s.fail(w, r, http.StatusBadRequest, "label too long")
		return Key{}, "", false
	}
	key, raw, err := newKey()
	if err != nil {
		s.internalError(w, r, "key generation", err)
		return Key{}, "", false
	}
	key.Label = req.Label
	return key, raw, true
}

// issueKey answers POST /admin/projects/{id}/keys (D-028): another key of the project, shown once.
func (s *Server) issueKey(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) || !s.requireLeader(w, r) {
		return
	}
	project, ok := s.pathID(w, r, "id", "project")
	if !ok {
		return
	}
	key, raw, ok := s.keyRequest(w, r)
	if !ok {
		return
	}
	key.ProjectID = project
	err := s.store.IssueKey(r.Context(), key)
	switch {
	case errors.Is(err, ErrNotFound):
		s.fail(w, r, http.StatusNotFound, "project not found")
		return
	case err != nil:
		s.internalError(w, r, "issue key", err, "project", project.String())
		return
	}
	s.audit(project, "key_issued", key.ID.String(), map[string]string{"key_id": key.ID.String(), "key_prefix": key.Prefix})
	logger.L.Info("API: key issued", "project", project.String(), "key_id", key.ID.String(), "key_prefix", key.Prefix)
	writeJSON(w, http.StatusCreated, protocol.IssuedKey{
		KeyID: key.ID.String(), ProjectID: project.String(), APIKey: raw, KeyPrefix: key.Prefix, Label: key.Label,
	})
}

// closeKeySessions closes every session on this node admitted with keyID (4002) and tells the
// other nodes to do the same.
func (s *Server) closeKeySessions(project, keyID uuid.UUID) int {
	s.tenants.MarkKeyRevoked(keyID) // before the kill: an open racing this revoke closes itself (router)
	n := s.sessions.KillWhere(session.Filter{KeyID: keyID}, protocol.CloseKeyRevoked, protocol.CloseReasonKeyRevoked)
	s.propagate(protocol.ReplicaControl{Type: protocol.TypeKeyRevoked, Project: project.String(), KeyID: keyID.String()})
	return n
}

// revokeKey answers DELETE /admin/keys/{id} (D-028): the key stops admitting and every session
// admitted with it closes 4002 on every node; other keys' sessions stay. 204.
func (s *Server) revokeKey(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) || !s.requireLeader(w, r) {
		return
	}
	id, ok := s.pathID(w, r, "id", "key")
	if !ok {
		return
	}
	project, err := s.store.RevokeKey(r.Context(), id)
	switch {
	case errors.Is(err, ErrNotFound):
		s.fail(w, r, http.StatusNotFound, "key not found")
		return
	case err != nil:
		s.internalError(w, r, "revoke key", err, "key_id", id.String())
		return
	}
	n := s.closeKeySessions(project, id)
	s.audit(project, "key_revoked", id.String(), map[string]string{"key_id": id.String(), "sessions_closed": strconv.Itoa(n)})
	logger.L.Info("API: key revoked", "project", project.String(), "key_id", id.String(), "sessions_closed", n)
	w.WriteHeader(http.StatusNoContent)
}

// rotateKey answers POST /admin/keys/{id}/rotate (D-028): issue a new key and revoke the old one in
// one step; the new key is returned once, the old key's sessions close 4002.
func (s *Server) rotateKey(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) || !s.requireLeader(w, r) {
		return
	}
	old, ok := s.pathID(w, r, "id", "key")
	if !ok {
		return
	}
	next, raw, ok := s.keyRequest(w, r)
	if !ok {
		return
	}
	project, label, err := s.store.RotateKey(r.Context(), old, next)
	switch {
	case errors.Is(err, ErrNotFound):
		s.fail(w, r, http.StatusNotFound, "key not found")
		return
	case err != nil:
		s.internalError(w, r, "rotate key", err, "key_id", old.String())
		return
	}
	n := s.closeKeySessions(project, old)
	s.audit(project, "key_rotated", old.String(), map[string]string{
		"key_id": old.String(), "new_key_id": next.ID.String(), "key_prefix": next.Prefix, "sessions_closed": strconv.Itoa(n),
	})
	logger.L.Info("API: key rotated", "project", project.String(), "key_id", old.String(),
		"new_key_id", next.ID.String(), "key_prefix", next.Prefix, "sessions_closed", n)
	writeJSON(w, http.StatusCreated, protocol.IssuedKey{
		KeyID: next.ID.String(), ProjectID: project.String(), APIKey: raw, KeyPrefix: next.Prefix, Label: label,
	})
}

func (s *Server) suspendProject(w http.ResponseWriter, r *http.Request) { s.setSuspended(w, r, true) }
func (s *Server) resumeProject(w http.ResponseWriter, r *http.Request)  { s.setSuspended(w, r, false) }

// setSuspended answers POST /admin/projects/{id}/suspend and /resume (D-027). Suspend: stored,
// then this node refuses opens and closes every session of the project (4001), then the other
// nodes are told. Resume lifts the refusal everywhere. Sheets and entries are untouched. 204.
func (s *Server) setSuspended(w http.ResponseWriter, r *http.Request, suspended bool) {
	if !s.adminOnly(w, r) || !s.requireLeader(w, r) {
		return
	}
	project, ok := s.pathID(w, r, "id", "project")
	if !ok {
		return
	}
	if _, err := s.store.SetSuspended(r.Context(), project, suspended); err != nil {
		if errors.Is(err, ErrNotFound) {
			s.fail(w, r, http.StatusNotFound, "project not found")
			return
		}
		s.internalError(w, r, "set suspended", err, "project", project.String())
		return
	}
	s.tenants.SetSuspended(project, suspended)
	if !suspended {
		s.propagate(protocol.ReplicaControl{Type: protocol.TypeProjectResumed, Project: project.String()})
		s.audit(project, "project_resumed", project.String(), nil)
		logger.L.Info("API: project resumed", "project", project.String())
		w.WriteHeader(http.StatusNoContent)
		return
	}
	n := s.sessions.KillWhere(session.Filter{Project: project}, protocol.CloseSuspended, protocol.CloseReasonSuspended)
	s.propagate(protocol.ReplicaControl{Type: protocol.TypeProjectSuspended, Project: project.String()})
	s.audit(project, "project_suspended", project.String(), map[string]string{"sessions_closed": strconv.Itoa(n)})
	logger.L.Info("API: project suspended", "project", project.String(), "sessions_closed", n)
	w.WriteHeader(http.StatusNoContent)
}
