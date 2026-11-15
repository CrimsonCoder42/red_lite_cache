// Covers: internal/api admin portal reads (S11 S5): GET /admin/stats merged across two nodes,
// GET /admin/audit filters + keyset, the admin session purge (internal/persist after-func)
package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/CrimsonCoder42/red_lite_cache/db/sqlc/audit"
	"github.com/CrimsonCoder42/red_lite_cache/internal/persist"
	"github.com/CrimsonCoder42/red_lite_cache/internal/session"
	"github.com/CrimsonCoder42/red_lite_cache/internal/sheet"
	"github.com/CrimsonCoder42/red_lite_cache/protocol"
)

// TestStatsMergedTwoNodes: each node answers its own entry plus the peer's (fetched with
// ?scope=local, so it does not recurse); a dead peer adds an error entry instead of failing.
func TestStatsMergedTwoNodes(t *testing.T) {
	muxA, muxB := http.NewServeMux(), http.NewServeMux()
	srvA, srvB := httptest.NewServer(muxA), httptest.NewServer(muxB)
	t.Cleanup(srvA.Close)
	t.Cleanup(srvB.Close)
	node := func(name, role string, sheets int, peer string, mux *http.ServeMux) {
		New(sheet.NewRegistry(sheet.Options{BcryptCost: 4}), NewMemStore(), admin, nil, Options{
			Sessions: session.NewRegistry(name), PeerURL: peer,
			Stats: func() NodeStats { return NodeStats{Role: role, Stats: &protocol.HealthStats{Sheets: sheets}} },
		}).Register(mux)
	}
	node("node-a", "leader", 3, srvB.URL, muxA)
	node("node-b", "replica", 5, srvA.URL, muxB)

	p := &portal{srv: srvA}
	var out StatsResponse
	r := p.do(t, nil, "GET", "/admin/stats", admin, nil)
	if r.status != http.StatusOK {
		t.Fatalf("stats: %d %s", r.status, r.body)
	}
	r.decode(t, &out)
	if len(out.Nodes) != 2 || out.Nodes[0].Node != "node-a" || out.Nodes[1].Node != "node-b" ||
		out.Nodes[0].Stats.Sheets != 3 || out.Nodes[1].Stats.Sheets != 5 || out.Nodes[1].Role != "replica" || out.Nodes[0].StartedAt == "" {
		t.Fatalf("merged stats = %s", r.body)
	}
	p.do(t, nil, "GET", "/admin/stats?scope=local", admin, nil).decode(t, &out)
	if len(out.Nodes) != 1 {
		t.Fatalf("scope=local returned %d nodes", len(out.Nodes))
	}
	srvB.Close()
	p.do(t, nil, "GET", "/admin/stats", admin, nil).decode(t, &out)
	if len(out.Nodes) != 2 || out.Nodes[1].Error == "" || out.Nodes[0].Stats == nil {
		t.Fatalf("with the peer down = %+v", out.Nodes)
	}
}

// TestAuditFilters: newest first, the project and action filters, since, and a keyset walk that
// returns every row exactly once even when rows share a timestamp.
func TestAuditFilters(t *testing.T) {
	p := newPortal(t, Options{})
	proj := uuid.New()
	base := time.Now().UTC().Truncate(time.Microsecond).Add(-time.Hour)
	ins := func(at time.Time, project uuid.UUID, action, subject string) {
		if err := p.store.Audit.InsertAuditEvent(t.Context(), audit.InsertAuditEventParams{
			At: at, Node: "n1", ProjectID: pgtype.UUID{Bytes: project, Valid: project != uuid.Nil},
			Actor: "admin", Action: action, Subject: subject, Detail: json.RawMessage(`{}`),
		}); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	for i := range 5 { // five rows at the same instant: the tiebreak must page them
		ins(base, proj, "key_issued", "k"+string(rune('a'+i)))
	}
	ins(base.Add(time.Minute), proj, "project_suspended", proj.String())
	ins(base.Add(2*time.Minute), uuid.Nil, "admin_login", "u1")

	get := func(q string) protocol.Page[AuditEventInfo] {
		var pg protocol.Page[AuditEventInfo]
		r := p.do(t, nil, "GET", "/admin/audit"+q, admin, nil)
		if r.status != http.StatusOK {
			t.Fatalf("audit%s: %d %s", q, r.status, r.body)
		}
		r.decode(t, &pg)
		return pg
	}
	all := get("")
	if len(all.Items) != 7 || all.Items[0].Action != "admin_login" || all.Items[1].Action != "project_suspended" {
		t.Fatalf("unfiltered: %+v", all.Items)
	}
	if pg := get("?project=" + proj.String()); len(pg.Items) != 6 {
		t.Fatalf("project filter: %d rows, want 6", len(pg.Items))
	}
	if pg := get("?action=key_issued"); len(pg.Items) != 5 {
		t.Fatalf("action filter: %d rows, want 5", len(pg.Items))
	}
	if pg := get("?since=" + base.Add(30*time.Second).Format(time.RFC3339Nano)); len(pg.Items) != 2 {
		t.Fatalf("since filter: %d rows, want 2", len(pg.Items))
	}
	seen := map[string]bool{}
	q := "?limit=2"
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("keyset walk did not end")
		}
		pg := get(q)
		for _, e := range pg.Items {
			if seen[e.Action+e.Subject] {
				t.Fatalf("row %s/%s returned twice", e.Action, e.Subject)
			}
			seen[e.Action+e.Subject] = true
		}
		if pg.NextCursor == "" {
			break
		}
		q = "?limit=2&cursor=" + pg.NextCursor
	}
	if len(seen) != 7 {
		t.Fatalf("keyset walk saw %d rows, want 7", len(seen))
	}
	if r := p.do(t, nil, "GET", "/admin/audit?cursor=bm9wZQ", admin, nil); r.status != http.StatusBadRequest {
		t.Fatalf("bad cursor: %d", r.status)
	}
}

// TestSessionsPurged: the cleaner after-func deletes expired and idle admin sessions and keeps a
// live one.
func TestSessionsPurged(t *testing.T) {
	p := newPortal(t, Options{})
	live, _ := p.bootstrapSuper(t)
	p.login(t, "root@example.test", testPassword)
	p.login(t, "root@example.test", testPassword)
	if n := p.count(t, "SELECT count(*) FROM admin_sessions"); n != 3 {
		t.Fatalf("sessions = %d, want 3", n)
	}
	keep := p.sessionCookie(live)
	p.exec(t, `UPDATE admin_sessions SET last_seen_at = now() - interval '31 minutes'
		WHERE token_hash = (SELECT token_hash FROM admin_sessions WHERE token_hash <> sha256($1::bytea) LIMIT 1)`, []byte(keep))
	p.exec(t, `UPDATE admin_sessions SET expires_at = now() - interval '1 second'
		WHERE token_hash <> sha256($1::bytea) AND last_seen_at > now() - interval '30 minutes'`, []byte(keep))
	persist.AdminSessionsPurge(p.store)(t.Context())
	if n := p.count(t, "SELECT count(*) FROM admin_sessions"); n != 1 {
		t.Fatalf("after purge %d sessions, want 1", n)
	}
	if r := p.do(t, live, "GET", "/admin/auth/me", "", nil); r.status != http.StatusOK {
		t.Fatalf("the live session after purge: %d", r.status)
	}
}
