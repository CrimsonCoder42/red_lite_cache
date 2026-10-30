package api

// Session routes (D-026, D-030): admin key only, answered on any node. A listing is this node's
// sessions plus the peer's (fetched with ?scope=local so it does not recurse), each tagged with the
// node that holds it; a kill of a session not on this node is forwarded to the peer.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/CrimsonCoder42/red_lite_cache/internal/logger"
	"github.com/CrimsonCoder42/red_lite_cache/internal/session"
	"github.com/CrimsonCoder42/red_lite_cache/protocol"
)

const (
	scopeLocal  = "local" // ?scope=local: this node only (the forwarded call)
	peerTimeout = 2 * time.Second
	peerPages   = 25 // at most 25 × maxPageLimit peer sessions are merged into one listing
)

// peer is the other node's management API, called with the same admin key.
type peer struct {
	base     string // http(s)://host:port
	adminKey string
	client   *http.Client
}

func newPeer(base, adminKey string) *peer {
	return &peer{base: strings.TrimRight(base, "/"), adminKey: adminKey, client: &http.Client{Timeout: peerTimeout}}
}

func (p *peer) do(ctx context.Context, method, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, p.base+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+p.adminKey)
	return p.client.Do(req)
}

// sessions pages through the peer's local sessions at path (a listing route) with extra query q.
func (p *peer) sessions(ctx context.Context, path string, q url.Values) ([]protocol.SessionInfo, error) {
	var out []protocol.SessionInfo
	q.Set("scope", scopeLocal)
	q.Set("limit", fmt.Sprint(maxPageLimit))
	for range peerPages {
		resp, err := p.do(ctx, http.MethodGet, path+"?"+q.Encode())
		if err != nil {
			return out, fmt.Errorf("api: peer sessions: %w", err)
		}
		var page protocol.Page[protocol.SessionInfo]
		err = json.NewDecoder(resp.Body).Decode(&page)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return out, fmt.Errorf("api: peer sessions: status %d", resp.StatusCode)
		}
		if err != nil {
			return out, fmt.Errorf("api: peer sessions: decode: %w", err)
		}
		out = append(out, page.Items...)
		if page.NextCursor == "" {
			return out, nil
		}
		q.Set("cursor", page.NextCursor)
	}
	return out, nil
}

// kill forwards DELETE /admin/sessions/{id}?scope=local and returns the peer's status.
func (p *peer) kill(ctx context.Context, id uuid.UUID) (int, error) {
	resp, err := p.do(ctx, http.MethodDelete, "/admin/sessions/"+id.String()+"?scope="+scopeLocal)
	if err != nil {
		return 0, fmt.Errorf("api: peer kill: %w", err)
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}

func sessionInfo(i session.Info) protocol.SessionInfo {
	out := protocol.SessionInfo{
		SessionID: i.ID.String(), ProjectID: i.Project.String(), Sheet: i.Sheet, Remote: i.Remote,
		ConnectedAt: fmtTime(i.ConnectedAt), LastSeen: fmtTime(i.LastSeen), Epoch: i.Epoch,
		FramesIn: i.FramesIn, FramesOut: i.FramesOut, Node: i.Node,
	}
	if i.KeyID != uuid.Nil {
		out.KeyID = i.KeyID.String()
	}
	return out
}

// listSessions answers GET /admin/sessions[?project=<id>] (D-026).
func (s *Server) listSessions(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) {
		return
	}
	var f session.Filter
	q := url.Values{}
	if v := r.URL.Query().Get("project"); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			s.fail(w, r, http.StatusBadRequest, "invalid project id")
			return
		}
		f.Project = id
		q.Set("project", v)
	}
	s.sessionPage(w, r, f, "/admin/sessions", q)
}

// listSheetSessions answers GET /admin/sheets/{name}/sessions.
func (s *Server) listSheetSessions(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) {
		return
	}
	name := r.PathValue("name")
	s.sessionPage(w, r, session.Filter{Sheet: name}, "/admin/sheets/"+url.PathEscape(name)+"/sessions", url.Values{})
}

// sessionPage merges this node's matching sessions with the peer's (unless ?scope=local), orders
// them by (connected_at, session_id) and answers one page. An unreachable peer is logged and the
// page carries this node's sessions only.
func (s *Server) sessionPage(w http.ResponseWriter, r *http.Request, f session.Filter, peerPath string, peerQuery url.Values) {
	limit, c, ok := s.pageParams(w, r)
	if !ok {
		return
	}
	type row struct {
		at   time.Time
		info protocol.SessionInfo
	}
	var rows []row
	seen := map[string]bool{}
	for _, i := range s.sessions.List(f) {
		si := sessionInfo(i)
		seen[si.SessionID] = true
		rows = append(rows, row{at: i.ConnectedAt, info: si})
	}
	if s.peer != nil && r.URL.Query().Get("scope") != scopeLocal {
		remote, err := s.peer.sessions(r.Context(), peerPath, peerQuery)
		if err != nil {
			logger.L.Warn("API: peer sessions unavailable", "peer", s.peer.base, "err", err)
		}
		for _, si := range remote {
			at, perr := time.Parse(time.RFC3339Nano, si.ConnectedAt)
			if perr != nil || seen[si.SessionID] {
				continue
			}
			seen[si.SessionID] = true
			rows = append(rows, row{at: at, info: si})
		}
	}
	slices.SortFunc(rows, func(a, b row) int {
		if k := a.at.Compare(b.at); k != 0 {
			return k
		}
		return strings.Compare(a.info.SessionID, b.info.SessionID)
	})
	page := protocol.Page[protocol.SessionInfo]{Items: []protocol.SessionInfo{}}
	for _, rw := range rows {
		if c.id != "" && !c.after(rw.at, rw.info.SessionID) {
			continue
		}
		if len(page.Items) == limit {
			last := page.Items[limit-1]
			at, _ := time.Parse(time.RFC3339Nano, last.ConnectedAt)
			page.NextCursor = cursor{at: at, id: last.SessionID}.encode()
			break
		}
		page.Items = append(page.Items, rw.info)
	}
	writeJSON(w, http.StatusOK, page)
}

// killSession answers DELETE /admin/sessions/{id} (D-030): a session on this node is closed with
// 4003 "kicked by admin" (204); otherwise the kill is forwarded to the peer (its status is
// returned; 502 when it cannot be reached); 404 when neither holds it. The node that closes the
// session writes the session_kick audit row.
func (s *Server) killSession(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) {
		return
	}
	id, ok := s.pathID(w, r, "id", "session")
	if !ok {
		return
	}
	if sess, ok := s.sessions.Get(id); ok {
		if sess.Kill(protocol.CloseKicked, protocol.CloseReasonKicked) {
			s.audit(sess.Project(), "session_kick", id.String(), map[string]string{
				"session": id.String(), "sheet": sess.Sheet(), "node": s.sessions.Node(),
			})
			logger.L.Info("API: session killed", "session", id.String(), "project", sess.Project().String(), "sheet", sess.Sheet())
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if s.peer == nil || r.URL.Query().Get("scope") == scopeLocal {
		s.fail(w, r, http.StatusNotFound, "session not found")
		return
	}
	status, err := s.peer.kill(r.Context(), id)
	switch {
	case err != nil:
		logger.L.Warn("API: peer kill failed", "session", id.String(), "peer", s.peer.base, "err", err)
		s.fail(w, r, http.StatusBadGateway, "peer unreachable")
	case status == http.StatusNoContent:
		logger.L.Info("API: session kill forwarded", "session", id.String(), "peer", s.peer.base)
		w.WriteHeader(http.StatusNoContent)
	case status == http.StatusNotFound:
		s.fail(w, r, http.StatusNotFound, "session not found")
	default:
		s.fail(w, r, http.StatusBadGateway, fmt.Sprintf("peer answered %d", status))
	}
}
