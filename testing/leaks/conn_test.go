// Covers: memory and goroutine bounds
package leaks

// Connection, session, tenant and persist-queue layer of the D-038 leak suite (L6, L7, L8, L10 and
// the TestEdgeConn… cases). Helpers are prefixed conn so they do not collide with the other
// agents' files in package leaks.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/CrimsonCoder42/red_lite_cache/internal/api"
	"github.com/CrimsonCoder42/red_lite_cache/internal/config"
	"github.com/CrimsonCoder42/red_lite_cache/internal/logger"
	"github.com/CrimsonCoder42/red_lite_cache/internal/persist"
	"github.com/CrimsonCoder42/red_lite_cache/internal/session"
	"github.com/CrimsonCoder42/red_lite_cache/internal/sheet"
	"github.com/CrimsonCoder42/red_lite_cache/internal/tenant"
	"github.com/CrimsonCoder42/red_lite_cache/protocol"
	"github.com/CrimsonCoder42/red_lite_cache/testing/integration"
)

const connAdminKey = "conn-leak-admin-key"

// connAdmin turns the management API on (admin key) with autocreate off: sheets exist only when
// a test creates them for a project.
func connAdmin(c *config.Config) { c.AdminKey = connAdminKey; c.SheetAutocreate = false }

// connStall keeps a peer that never reads alive: no ping round trip inside the test, and a frame
// write may block (full socket buffers) for longer than the test without timing out.
func connStall(c *config.Config) {
	c.ConnSendQueue = 256 // ≥ 203 frames per stalled conn, so none is kicked for backpressure
	c.PingInterval = time.Minute
	c.WriteTimeout = time.Minute
}

// connAdminDo calls srv's management API with the admin key and returns the status.
func connAdminDo(t *testing.T, srv *integration.Server, method, path string) int {
	t.Helper()
	req, _ := http.NewRequestWithContext(t.Context(), method, srv.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+connAdminKey)
	req.Close = true // no idle keep-alive conn: its client + server goroutines would count against the baseline
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// connProject registers a project (one key) in srv's project store and n sheets (password "pw").
func connProject(t *testing.T, srv *integration.Server, n int) (uuid.UUID, []string) {
	t.Helper()
	p := uuid.New()
	raw := "rlk_" + uuid.NewString()
	k := api.Key{ID: uuid.New(), ProjectID: p, Hash: tenant.HashKey(raw), Prefix: raw[:8]}
	if err := srv.App.Projects().CreateProject(context.Background(), p, "p-"+p.String(), k); err != nil {
		t.Fatalf("create project: %v", err)
	}
	names := make([]string, n)
	for i := range names {
		names[i] = integration.UniqueSheet(t)
		if _, err := srv.App.Registry().Create(p, names[i], "pw"); err != nil {
			t.Fatalf("create sheet: %v", err)
		}
	}
	return p, names
}

// connLog points logger.L at a buffer for the test (restored at cleanup). Only for tests that run
// no server: a server's goroutines read logger.L.
func connLog(t *testing.T) *connBuf {
	t.Helper()
	buf := &connBuf{}
	prev := logger.L
	logger.L = logger.New(buf, slog.LevelDebug, false)
	t.Cleanup(func() { logger.L = prev })
	return buf
}

type connBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (c *connBuf) Write(p []byte) (int, error) { c.mu.Lock(); defer c.mu.Unlock(); return c.b.Write(p) }

// count is the number of log lines containing every one of subs.
func (c *connBuf) count(subs ...string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for line := range strings.SplitSeq(c.b.String(), "\n") {
		ok := line != ""
		for _, s := range subs {
			ok = ok && strings.Contains(line, s)
		}
		if ok {
			n++
		}
	}
	return n
}

// TestL6ConnCloseFreesQueueAndGoroutines: 200 peers that stop reading each hold ~200 queued
// broadcast frames, a lock and a session; closing them frees every goroutine, session, conn and
// lock.
func TestL6ConnCloseFreesQueueAndGoroutines(t *testing.T) {
	const peers, frames = 200, 200
	st := NewSteps(t)
	srv := integration.StartLeader(t, connStall) // password mode, autocreate on (development)
	name := integration.UniqueSheet(t)
	writer, _ := integration.OpenRaw(t, srv, name)
	writerSess := srv.App.Sessions().List(session.Filter{Sheet: name})[0].ID

	base := integration.GoroutineBaseline(t)
	st.Baseline("goroutines=%d sessions=%d conns=%d locks=%d", base, srv.App.Sessions().Len(), srv.App.Conns(), srv.App.Locks().Len())

	conns := make([]*integration.RawConn, peers)
	for i := range conns {
		conns[i], _ = integration.OpenRaw(t, srv, name)
		if r := conns[i].Do(protocol.Request{Action: protocol.ActionLock, ID: 1, Sheet: name, Key: fmt.Sprintf("lk%d", i)}); !r.Success {
			t.Fatalf("lock %d: %+v", i, r)
		}
	}
	afterOpen := integration.Goroutines(t)
	// 32 KiB values: socket buffers absorb only a handful per peer, the rest wait in the send queue.
	value := json.RawMessage(`"` + strings.Repeat("x", 32<<10) + `"`)
	for i := range frames {
		if r := writer.Do(protocol.Request{Action: protocol.ActionSet, ID: uint64(i + 2), Key: fmt.Sprintf("k%d", i), Value: value}); !r.Success {
			t.Fatalf("set %d: %+v", i, r)
		}
	}
	st.Fill("%d stalled peers, %d broadcasts of %d B each", peers, frames, len(value))

	peak := integration.Goroutines(t)
	// Frames accepted minus frames written, per stalled peer: open reply + sheet_state + lock reply
	// + every broadcast (no kick: 203 < ConnSendQueue). It is the send queue plus at most one frame
	// mid-write per peer (ws.Handler.QueuedFrames is not reachable through app.App yet).
	pending := 0
	for _, s := range srv.App.Sessions().List(session.Filter{Sheet: name}) {
		if s.ID != writerSess {
			pending += 3 + frames - int(s.FramesOut)
		}
	}
	st.Observe("goroutines=%d (after open %d, +%.2f per peer) frames pending=%d sessions=%d conns=%d locks=%d",
		peak, afterOpen, float64(afterOpen-base)/peers, pending, srv.App.Sessions().Len(), srv.App.Conns(), srv.App.Locks().Len())
	Above(t, "goroutines", peak, base)
	if peak < base+2*peers {
		t.Fatalf("OBSERVE goroutines %d < baseline %d + 2×%d (server read + writer per conn)", peak, base, peers)
	}
	Above(t, "frames pending in send queues", pending, 0)
	Above(t, "sessions", srv.App.Sessions().Len(), 1)
	Above(t, "locks", srv.App.Locks().Len(), 0)

	for _, c := range conns {
		c.Conn.CloseNow()
	}
	st.Drain("closed %d stalled peers", peers)

	got := integration.WaitGoroutines(t, base, 0)
	integration.Eventually(t, "sessions, conns and locks back to the writer's", func() bool {
		return srv.App.Sessions().Len() == 1 && srv.App.Conns() == 1 && srv.App.Locks().Len() == 0
	})
	writer.Conn.CloseNow()
	integration.Eventually(t, "no session, conn or collaborator left", func() bool {
		return srv.App.Sessions().Len() == 0 && srv.App.Conns() == 0 && srv.App.Registry().Stats().Collaborators == 0
	})
	final := integration.WaitGoroutines(t, base, 0)
	st.Assert("goroutines after peers closed=%d, after writer closed=%d (baseline %d) sessions=%d conns=%d locks=%d wall=%s",
		got, final, base, srv.App.Sessions().Len(), srv.App.Conns(), srv.App.Locks().Len(), st.Elapsed().Round(time.Millisecond))
	// Tolerance 0 after the peers close: every goroutine a peer added must be gone. After the writer
	// also closes, its own server goroutines go too (it was opened before the baseline).
	AtMost(t, "goroutines after peers closed", got, base, 0)
	if final > base {
		t.Errorf("ASSERT goroutines after writer closed %d > baseline %d", final, base)
	}
}

// TestL7SessionKickAndSuspendFree: 50 sessions over 5 projects; 10 kicked, 2 projects suspended,
// 1 revoked: exactly the 10 survivors keep a session and goroutines.
func TestL7SessionKickAndSuspendFree(t *testing.T) {
	const perSheet = 5
	st := NewSteps(t)
	srv := integration.StartLeader(t, connAdmin)
	projects := make([]uuid.UUID, 5)
	sheets := make([][]string, 5)
	for i := range projects {
		projects[i], sheets[i] = connProject(t, srv, 2)
	}
	base := integration.GoroutineBaseline(t)
	st.Baseline("goroutines=%d sessions=%d tenants=%+v", base, srv.App.Sessions().Len(), srv.App.Tenants().Stats())

	var conns []*integration.RawConn
	for i := range projects {
		for _, name := range sheets[i] {
			for range perSheet {
				c, _ := integration.OpenRaw(t, srv, name)
				// A peer that keeps reading (like the reader): it answers pings and the close
				// handshake, so a kill frees the session at once. A peer that never reads holds a
				// killed session ~5 s (coder/websocket close-handshake wait) — TestEdgeConnKickMidOpen.
				c.Conn.CloseRead(context.Background())
				conns = append(conns, c)
			}
		}
	}
	st.Fill("%d sessions over %d projects × 2 sheets", len(conns), len(projects))
	peak := integration.Goroutines(t)
	perConn := (peak - base) / len(conns)
	st.Observe("goroutines=%d (+%d, %d per session) sessions=%d", peak, peak-base, perConn, srv.App.Sessions().Len())
	Above(t, "goroutines", peak, base)
	Above(t, "sessions", srv.App.Sessions().Len(), 0)
	if (peak-base)%len(conns) != 0 {
		t.Fatalf("OBSERVE goroutines +%d is not a whole number per session (%d)", peak-base, len(conns))
	}

	// kick 5 sessions of project 0 and 5 of project 4 through the API
	for _, p := range []uuid.UUID{projects[0], projects[4]} {
		for _, s := range srv.App.Sessions().List(session.Filter{Project: p})[:5] {
			if code := connAdminDo(t, srv, "DELETE", "/admin/sessions/"+s.ID.String()); code != http.StatusNoContent {
				t.Fatalf("kick: %d", code)
			}
		}
	}
	for _, p := range projects[1:3] {
		if code := connAdminDo(t, srv, "POST", "/admin/projects/"+p.String()+"/suspend"); code != http.StatusNoContent {
			t.Fatalf("suspend: %d", code)
		}
	}
	if code := connAdminDo(t, srv, "POST", "/admin/projects/"+projects[3].String()+"/revoke"); code != http.StatusNoContent {
		t.Fatalf("revoke: %d", code)
	}
	st.Drain("kicked 10, suspended 2 projects (20 sessions), revoked 1 project (10 sessions, 2 sheets)")

	const survivors = 50 - 10 - 20 - 10
	integration.Eventually(t, "only the survivors keep a session", func() bool {
		return srv.App.Sessions().Len() == survivors && srv.App.Conns() == survivors
	})
	want := base + survivors*perConn
	got := integration.WaitGoroutines(t, want, 0)
	ts := srv.App.Tenants().Stats()
	st.Assert("sessions=%d conns=%d tenants=%+v goroutines=%d (want baseline %d + %d×%d = %d) wall=%s",
		srv.App.Sessions().Len(), srv.App.Conns(), ts, got, base, survivors, perConn, want, st.Elapsed().Round(time.Millisecond))
	if ts.Suspended != 2 || ts.RevokedNames != 2 {
		t.Errorf("tenant stats %+v, want Suspended 2, RevokedNames 2", ts)
	}
	// tolerance 0: a killed session's server goroutines all exit; the survivors keep exactly theirs
	Within(t, "goroutines", got, want, 0)
	for _, c := range conns {
		c.Conn.CloseNow()
	}
	integration.Eventually(t, "every session gone after the clients close", func() bool { return srv.App.Sessions().Len() == 0 })
	AtMost(t, "goroutines after every client closed", integration.WaitGoroutines(t, base, 0), base, 0)
}

// TestL8TenantSetsBounded: the revoked-name and dead-key sets (D-041) keep the newest cap members,
// warn once per set on the first drop, and empty on ResetRevocations (the Postgres reload; with
// persistence off nothing calls it — the sets are then the only memory of a revoke and stay bounded).
func TestL8TenantSetsBounded(t *testing.T) {
	const maxN, n = 10_000, 12_000
	st := NewSteps(t)
	logs := connLog(t)
	d := tenant.New(nil, false)
	d.SetMemoryMax(maxN)
	st.Baseline("%+v", d.Stats())

	keys := make([]uuid.UUID, n)
	names := make([]string, n)
	for i := range n {
		keys[i] = uuid.New()
		names[i] = fmt.Sprintf("sheet-%d", i)
		d.MarkKeyRevoked(keys[i])
		d.MarkRevoked(names[i])
	}
	st.Fill("%d key ids and %d sheet names marked revoked", n, n)
	s := d.Stats()
	st.Observe("%+v", s)
	Above(t, "dead keys", s.DeadKeys, 0)
	Above(t, "revoked names", s.RevokedNames, 0)
	if s.DeadKeys != maxN || s.RevokedNames != maxN || s.KeysDropped != n-maxN || s.NamesDropped != n-maxN {
		t.Fatalf("stats %+v, want %d held and %d dropped per set", s, maxN, n-maxN)
	}
	for i := range n {
		if want := i >= n-maxN; d.KeyRevoked(keys[i]) != want || d.Revoked(names[i]) != want {
			t.Fatalf("member %d: key %v name %v, want %v (oldest %d dropped)", i, d.KeyRevoked(keys[i]), d.Revoked(names[i]), want, n-maxN)
		}
	}
	keyWarns := logs.count("API: tenancy memory bound reached", "set=dead_key_ids")
	nameWarns := logs.count("API: tenancy memory bound reached", "set=revoked_sheet_names")
	if keyWarns != 1 || nameWarns != 1 {
		t.Fatalf("warns: dead keys %d, names %d, want exactly 1 each", keyWarns, nameWarns)
	}

	d.ResetRevocations()
	st.Drain("ResetRevocations")
	s = d.Stats()
	st.Assert("%+v wall=%s", s, st.Elapsed().Round(time.Millisecond))
	if s.DeadKeys != 0 || s.RevokedNames != 0 || d.KeyRevoked(keys[n-1]) || d.Revoked(names[n-1]) {
		t.Fatalf("after reset %+v", s)
	}
}

// TestL10PersistQueueBound: with the flusher stopped, the persist queue holds QueueSize ops and
// drops the rest with a rate-limited Warn; a sheet create parks instead of dropping; once the
// writer runs (stub flush, first batch failing once so a held batch exists), everything drains.
func TestL10PersistQueueBound(t *testing.T) {
	const queueSize, ops, sheets = 1_000, 5_000, 10
	st := NewSteps(t)
	logs := connLog(t)
	calls := 0
	release := make(chan struct{})
	var mu sync.Mutex
	w := persist.NewWriterFlush(persist.Options{QueueSize: queueSize, BatchSize: 100, FlushInterval: 10 * time.Millisecond},
		func(ctx context.Context, _ int, _ uint64) error {
			mu.Lock()
			calls++
			c := calls
			mu.Unlock()
			switch c {
			case 1:
				return fmt.Errorf("stub: first flush fails")
			case 2: // the retry of the held batch waits until the test has seen it held
				select {
				case <-release:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return nil
		}, nil)
	base := integration.GoroutineBaseline(t)
	st.Baseline("goroutines=%d %+v", base, w.Stats())

	project := uuid.New()
	for i := range ops {
		w.Observe(sheet.Op{Kind: sheet.OpSet, Sheet: fmt.Sprintf("s%d", i%sheets), ProjectID: project,
			Key: fmt.Sprintf("k%d", i), Value: json.RawMessage(`1`)})
	}
	for i := range sheets { // the queue is full: every create parks
		w.Observe(sheet.Op{Kind: sheet.OpCreate, Sheet: fmt.Sprintf("s%d", i), ProjectID: project})
	}
	st.Fill("%d sets over %d sheets + %d creates, flusher stopped", ops, sheets, sheets)
	s := w.Stats()
	warns := logs.count("Persist: enqueue dropped")
	st.Observe("%+v warns=%d", s, warns)
	Above(t, "queued", s.Queued, 0)
	Above(t, "parked", s.Parked, 0)
	if s.Queued > queueSize || s.Dropped != ops-queueSize || s.Parked > sheets {
		t.Fatalf("stats %+v, want Queued ≤ %d, Dropped %d, Parked ≤ %d", s, queueSize, ops-queueSize, sheets)
	}
	if maxWarns := (ops - queueSize + queueSize - 1) / queueSize; warns < 1 || warns > maxWarns {
		t.Fatalf("enqueue-dropped warns = %d, want 1..%d (never one per op)", warns, maxWarns)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	integration.Eventually(t, "a failed batch held for its retry", func() bool { return w.Stats().Held > 0 })
	held := w.Stats().Held
	close(release)
	st.Drain("writer running (held batch %d ops released)", held)
	integration.Eventually(t, "queue, parked and held empty", func() bool {
		s := w.Stats()
		return s.Queued == 0 && s.Parked == 0 && s.Held == 0
	})
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	s = w.Stats()
	got := integration.WaitGoroutines(t, base, 0)
	st.Assert("%+v goroutines=%d (baseline %d) wall=%s", s, got, base, st.Elapsed().Round(time.Millisecond))
	if s.Queued != 0 || s.Parked != 0 || s.Held != 0 {
		t.Errorf("after drain %+v", s)
	}
	AtMost(t, "goroutines", got, base, 0) // the Run goroutine is the only one this test started
}

// TestL10PersistQueueBoundPostgres: the real-database half — a leader with persistence drains its
// writer (queue, parked creates, held batch) after a burst of writes.
func TestL10PersistQueueBoundPostgres(t *testing.T) {
	dbURL := connPersistURL(t)
	st := NewSteps(t)
	srv := integration.StartLeader(t, func(c *config.Config) {
		c.PersistEnabled = true
		c.PersistDatabaseURL = dbURL
		c.PersistFlushInterval = 50 * time.Millisecond
		c.PersistCleanupInterval = time.Hour
		c.PersistPoolMin, c.PersistPoolMax = 1, 2
	})
	w := srv.App.Writer()
	st.Baseline("%+v", w.Stats())
	c, _ := integration.OpenRaw(t, srv, integration.UniqueSheet(t))
	for i := range 500 {
		c.Send(protocol.Request{Action: protocol.ActionSet, ID: uint64(i + 1), Key: fmt.Sprintf("k%d", i), Value: json.RawMessage(`1`)})
	}
	for range 500 {
		c.Reply()
	}
	st.Fill("500 sets")
	st.Observe("%+v", w.Stats())
	integration.Eventually(t, "writer drained", func() bool {
		s := w.Stats()
		return s.Queued == 0 && s.Parked == 0 && s.Held == 0
	})
	st.Drain("flushed")
	s := w.Stats()
	st.Assert("%+v wall=%s", s, st.Elapsed().Round(time.Millisecond))
	if s.Dropped != 0 {
		t.Errorf("dropped %d with a 10 000 queue and 500 ops", s.Dropped)
	}
}

// connPersistURL creates a schema for this test on REDLITE_TEST_DATABASE_URL and returns the URL
// bound to it; the schema is dropped at cleanup. Skips with integration.SkipPersist without it.
// (same shape as testing/integration persistURL, which is test-only there)
func connPersistURL(t *testing.T) string {
	t.Helper()
	base := integration.TestDatabaseURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), integration.Timeout)
	defer cancel()
	schema := fmt.Sprintf("rl_leak_%d", time.Now().UnixNano())
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), integration.Timeout)
		defer cancel()
		if _, err := admin.Exec(cctx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Errorf("drop schema %s: %v", schema, err)
		}
		admin.Close(cctx)
	})
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String()
}

// TestEdgeConnKickMidOpen: sessions killed while their opens are in flight (between admission,
// sessions.Add and sheet.Join) leave no session, conn, collaborator or goroutine behind.
func TestEdgeConnKickMidOpen(t *testing.T) {
	const clients = 40
	st := NewSteps(t)
	srv := integration.StartLeader(t, connAdmin)
	p, names := connProject(t, srv, 1)
	base := integration.GoroutineBaseline(t)
	st.Baseline("goroutines=%d sessions=%d", base, srv.App.Sessions().Len())

	open, _ := json.Marshal(protocol.Request{Action: protocol.ActionOpen, ID: 1, Sheet: names[0], Password: "pw"})
	var wg sync.WaitGroup
	stop := make(chan struct{})
	kills := 0
	killer := make(chan struct{})
	go func() { // kill the project's sessions in a loop while the opens run
		defer close(killer)
		for {
			select {
			case <-stop:
				return
			default:
				kills += srv.App.Sessions().KillWhere(session.Filter{Project: p}, protocol.CloseKicked, protocol.CloseReasonKicked)
			}
		}
	}()
	socks := make([]*websocket.Conn, clients)
	for i := range socks {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(context.Background(), integration.Timeout)
			defer cancel()
			c, _, err := websocket.Dial(ctx, srv.WSURL, nil)
			if err != nil {
				return
			}
			socks[i] = c
			if c.Write(ctx, websocket.MessageText, open) == nil {
				c.Read(ctx) // the open reply, or the kick's close
			}
		})
	}
	wg.Wait()
	peak := srv.App.Conns()
	close(stop)
	<-killer
	kills += srv.App.Sessions().KillWhere(session.Filter{Project: p}, protocol.CloseKicked, protocol.CloseReasonKicked)
	st.Fill("%d concurrent opens under a kill loop", clients)
	st.Observe("conns=%d kills=%d", peak, kills)
	Above(t, "sessions killed", kills, 0)

	st.Drain("a final KillWhere after every open returned")
	// The clients stopped reading after one frame, so each killed conn waits out coder/websocket's
	// 5 s close-handshake read before its socket closes (see L7): the Eventually below covers it.
	integration.Eventually(t, "no session, conn or collaborator left", func() bool {
		return srv.App.Sessions().Len() == 0 && srv.App.Conns() == 0 && srv.App.Registry().Stats().Collaborators == 0
	})
	for _, c := range socks {
		if c != nil {
			c.CloseNow()
		}
	}
	got := integration.WaitGoroutines(t, base, 0)
	st.Assert("sessions=%d conns=%d goroutines=%d (baseline %d) wall=%s", srv.App.Sessions().Len(), srv.App.Conns(), got, base, st.Elapsed().Round(time.Millisecond))
	AtMost(t, "goroutines", got, base, 0)
}

// TestEdgeConnSuspendZeroSessions: suspending a project with no sessions marks it suspended,
// closes nothing and does not panic.
func TestEdgeConnSuspendZeroSessions(t *testing.T) {
	st := NewSteps(t)
	srv := integration.StartLeader(t, connAdmin)
	p, _ := connProject(t, srv, 1)
	before := srv.App.Tenants().Stats()
	st.Baseline("%+v sessions=%d", before, srv.App.Sessions().Len())
	if code := connAdminDo(t, srv, "POST", "/admin/projects/"+p.String()+"/suspend"); code != http.StatusNoContent {
		t.Fatalf("suspend: %d", code)
	}
	st.Fill("suspended a project with zero sessions")
	after := srv.App.Tenants().Stats()
	st.Observe("%+v sessions=%d", after, srv.App.Sessions().Len())
	Above(t, "suspended", after.Suspended, before.Suspended)
	if code := connAdminDo(t, srv, "POST", "/admin/projects/"+p.String()+"/resume"); code != http.StatusNoContent {
		t.Fatalf("resume: %d", code)
	}
	st.Drain("resumed")
	final := srv.App.Tenants().Stats()
	st.Assert("%+v sessions=%d", final, srv.App.Sessions().Len())
	if after.Suspended != before.Suspended+1 || srv.App.Sessions().Len() != 0 || final.Suspended != before.Suspended {
		t.Fatalf("suspended %d→%d→%d sessions %d", before.Suspended, after.Suspended, final.Suspended, srv.App.Sessions().Len())
	}
}
