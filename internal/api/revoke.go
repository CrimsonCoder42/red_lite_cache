package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/google/uuid"

	"github.com/CrimsonCoder42/red_lite_cache/db"
	"github.com/CrimsonCoder42/red_lite_cache/internal/logger"
	"github.com/CrimsonCoder42/red_lite_cache/internal/persist"
	"github.com/CrimsonCoder42/red_lite_cache/internal/session"
	"github.com/CrimsonCoder42/red_lite_cache/internal/sheet"
	"github.com/CrimsonCoder42/red_lite_cache/protocol"
)

// Revoker is a Store that can revoke a project: every live key revoked and every sheet deleted,
// returning the deleted sheet names. MemStore and DBStore implement it.
type Revoker interface {
	RevokeProject(ctx context.Context, id uuid.UUID) (sheets []string, err error)
}

// RevokeProject revokes every key of the project. MemStore keeps no sheets (the registry is the
// only copy), so it returns none; the route drops the registry's sheets of the project itself.
func (m *MemStore) RevokeProject(_ context.Context, id uuid.UUID) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.projects[id]; !ok {
		return nil, ErrNotFound
	}
	now := storeNow()
	for _, k := range m.keys {
		if k.ProjectID == id && k.RevokedAt.IsZero() {
			k.RevokedAt = now
		}
	}
	return nil, nil
}

// RevokeProject runs RevokeProjectKeys + DeleteProjectSheets (entries cascade) in one
// transaction, then publishes project_revoked on the control bus (D-021) so every other instance
// drops the sheets from memory. A failed NOTIFY is logged: the cleaner-tick reconcile covers it.
func (d *DBStore) RevokeProject(ctx context.Context, id uuid.UUID) ([]string, error) {
	exists, err := d.ProjectExists(ctx, id)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrNotFound
	}
	var names []string
	if err := d.store.ExecTx(ctx, func(tx *db.Store) error {
		if _, err := tx.Registry.RevokeProjectKeys(ctx, id); err != nil {
			return fmt.Errorf("api: revoke keys %s: %w", id, err)
		}
		n, err := tx.Registry.DeleteProjectSheets(ctx, id)
		if err != nil {
			return fmt.Errorf("api: delete sheets %s: %w", id, err)
		}
		names = n
		return nil
	}); err != nil {
		return nil, err
	}
	if err := persist.Notify(ctx, d.store, persist.Control{Kind: persist.KindProjectRevoked, Project: id.String()}); err != nil {
		logger.L.Error("API: control notify failed", "project", id.String(), "err", err)
	}
	return names, nil
}

// revokeProject answers POST /admin/projects/{id}/revoke (admin key; leader only): revoke every key
// of the project, delete its sheets, drop them from this registry and kick their collaborators.
// 204 on success.
func (s *Server) revokeProject(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authenticate(r, false); !ok {
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
	rv, ok := s.store.(Revoker)
	if !ok {
		s.fail(w, r, http.StatusNotImplemented, "revoke not supported")
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
	// Memory first, then the writer, then Postgres: once the sheets are out of the registry no new
	// op of theirs can be accepted; the writer then discards what it holds and waits out a flush in
	// flight; only after that are the rows deleted, so no queued or parked op re-inserts them.
	// Terminal close first (4002: the project's keys are being revoked), so the readers stop instead
	// of reconnecting into "not authorized"; the drop's ordinary kick after it is then a no-op
	// (CloseWith: the first close wins). The other node gets the same through the control frame.
	s.sessions.KillWhere(session.Filter{Project: project}, protocol.CloseKeyRevoked, protocol.CloseReasonKeyRevoked)
	s.propagate(protocol.ReplicaControl{Type: protocol.TypeKeyRevoked, Project: project.String()})
	dropped := DropProject(s.reg, project, nil)
	s.revoked(project, dropped)
	names, err := rv.RevokeProject(r.Context(), project)
	switch {
	case errors.Is(err, ErrNotFound):
		s.fail(w, r, http.StatusNotFound, "project not found")
		return
	case err != nil:
		logger.L.Error("API: revoke failed", "project", project.String(), "err", err)
		s.fail(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	if late := DropProject(s.reg, project, names); len(late) > 0 { // in Postgres, not in memory
		s.revoked(project, late)
		dropped = append(dropped, late...)
	}
	s.tenants.MarkRevoked(dropped...) // autocreate never recreates them (D-025)
	s.audit(project, "project_revoked", project.String(), map[string]string{"sheets_dropped": strconv.Itoa(len(dropped))})
	logger.L.Info("API: project revoked", "project", project.String(), "sheets_dropped", len(dropped))
	w.WriteHeader(http.StatusNoContent)
}

// OnRevoke registers fn, called on this server's revoke after the sheets left the registry and
// before Postgres deletes them. Call before serving; not safe concurrently with requests.
func (s *Server) OnRevoke(fn func(project uuid.UUID, sheets []string)) {
	s.onRevoke = append(s.onRevoke, fn)
}

func (s *Server) revoked(project uuid.UUID, sheets []string) {
	for _, fn := range s.onRevoke {
		fn(project, sheets)
	}
}

// DropProject removes from reg every named sheet and every sheet owned by project, kicking their
// collaborators, and returns the names it removed. The revoke route and the control-bus listener
// (D-021) both use it, so a leader and its replicas drop the same set.
func DropProject(reg *sheet.Registry, project uuid.UUID, names []string) []string {
	var out []string
	for _, name := range names {
		if reg.Drop(name, "project revoked") {
			out = append(out, name)
		}
	}
	for _, s := range reg.Sheets() {
		if s.ProjectID() == project && reg.Drop(s.Name(), "project revoked") {
			out = append(out, s.Name())
		}
	}
	return out
}
