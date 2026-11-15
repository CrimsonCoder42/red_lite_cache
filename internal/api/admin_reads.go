package api

// Admin portal reads (S11 S5): GET /admin/stats (this node's counts, merged with the peer like the
// session listing, ?scope=local stops the recursion) and GET /admin/audit (keyset over audit_events).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/CrimsonCoder42/red_lite_cache/internal/logger"
	"github.com/CrimsonCoder42/red_lite_cache/protocol"
)

const maxActionLen = 64

// statsHandler answers GET /admin/stats. A peer that cannot be read adds {node, error} instead of
// failing the page.
func (s *Server) statsHandler(w http.ResponseWriter, r *http.Request) {
	out := StatsResponse{Nodes: []NodeStats{s.localStats()}}
	if s.peer != nil && r.URL.Query().Get("scope") != scopeLocal {
		remote, err := s.peer.stats(r.Context())
		if err != nil {
			out.Nodes = append(out.Nodes, NodeStats{Node: s.peer.base, Error: "peer unreachable"})
			logger.L.Warn("API: peer stats unavailable", "peer", s.peer.base, "err", err)
		} else {
			out.Nodes = append(out.Nodes, remote...)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) localStats() NodeStats {
	var n NodeStats
	if s.stats != nil {
		n = s.stats()
	}
	n.Node = s.sessions.Node()
	n.StartedAt = fmtTime(s.startedAt)
	return n
}

// stats reads the peer's own entry (GET /admin/stats?scope=local, with the admin key).
func (p *peer) stats(ctx context.Context) ([]NodeStats, error) {
	resp, err := p.do(ctx, http.MethodGet, "/admin/stats?scope="+scopeLocal)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("peer answered %d", resp.StatusCode)
	}
	var out StatsResponse
	if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, maxBody)).Decode(&out); err != nil {
		return nil, err
	}
	return out.Nodes, nil
}

// auditHandler answers GET /admin/audit?since&project&action&limit&cursor: newest first.
func (s *Server) auditHandler(w http.ResponseWriter, r *http.Request) {
	limit, c, ok := s.pageParams(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	f := AuditFilter{Limit: limit + 1, Action: q.Get("action")}
	if v := q.Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			s.fail(w, r, http.StatusBadRequest, "invalid since")
			return
		}
		f.Since = t
	}
	if v := q.Get("project"); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			s.fail(w, r, http.StatusBadRequest, "invalid project id")
			return
		}
		f.Project = id
	}
	if len(f.Action) > maxActionLen {
		s.fail(w, r, http.StatusBadRequest, "invalid action")
		return
	}
	if c.id != "" { // the cursor id is node \x1f action \x1f subject
		parts := strings.SplitN(c.id, "\x1f", 3)
		if len(parts) != 3 {
			s.fail(w, r, http.StatusBadRequest, "invalid cursor")
			return
		}
		f.HasCursor, f.CurAt, f.CurNode, f.CurAction, f.CurSubject = true, c.at, parts[0], parts[1], parts[2]
	}
	rows, err := s.admins.AuditPage(r.Context(), f)
	if err != nil {
		s.internalError(w, r, "audit page", err)
		return
	}
	page := protocol.Page[AuditEventInfo]{Items: make([]AuditEventInfo, 0, min(len(rows), limit))}
	for i, e := range rows {
		if i == limit {
			last := rows[limit-1]
			page.NextCursor = cursor{at: last.At, id: last.Node + "\x1f" + last.Action + "\x1f" + last.Subject}.encode()
			break
		}
		info := AuditEventInfo{At: fmtTime(e.At), Node: e.Node, Actor: e.Actor, Action: e.Action, Subject: e.Subject, Detail: e.Detail}
		if e.ProjectID.Valid {
			info.ProjectID = uuid.UUID(e.ProjectID.Bytes).String()
		}
		page.Items = append(page.Items, info)
	}
	writeJSON(w, http.StatusOK, page)
}
