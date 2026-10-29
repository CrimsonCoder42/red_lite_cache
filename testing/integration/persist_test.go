// Covers: internal/persist (write-behind, hydration, epoch) through a running leader
package integration

import (
	"context"
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/CrimsonCoder42/red_lite_cache/internal/config"
	"github.com/CrimsonCoder42/red_lite_cache/protocol"
)

// persistURL creates a schema for this test on REDLITE_TEST_DATABASE_URL and returns the URL bound
// to it (search_path); the schema is dropped at cleanup. Skips with SkipPersist without the variable.
func persistURL(t *testing.T) string {
	t.Helper()
	base := TestDatabaseURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), Timeout)
	defer cancel()
	schema := fmt.Sprintf("rl_it_%d", time.Now().UnixNano())
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), Timeout)
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

// withPersist turns persistence on against dbURL with a fast flush and slow cleanup loops (so an
// expired row stays in the table for the hydration check).
func withPersist(dbURL string) func(*config.Config) {
	return func(c *config.Config) {
		c.PersistEnabled = true
		c.PersistDatabaseURL = dbURL
		c.PersistFlushInterval = 50 * time.Millisecond
		c.PersistCleanupInterval = time.Hour
		c.PersistPoolMin, c.PersistPoolMax = 1, 2
	}
}

func rowCount(t *testing.T, srv *Server, sheetName string) int {
	t.Helper()
	var n int
	err := srv.App.Store().Pool().QueryRow(t.Context(), "SELECT count(*) FROM cache_entries WHERE sheet = $1", sheetName).Scan(&n)
	if err != nil {
		t.Fatalf("count rows: %v", err)
	}
	return n
}

// TestPersistLoop (PERSIST loop): set → flush → restart on the same database → hydrated; the
// expired row is not loaded; the password is still required; the epoch survives the restart.
func TestPersistLoop(t *testing.T) {
	dbURL := persistURL(t)
	slowJanitor := func(c *config.Config) { c.CacheTTL = 300 * time.Millisecond; c.CacheJanitorInterval = time.Hour }
	first := StartLeader(t, withPersist(dbURL), slowJanitor)
	if e := first.App.Node().Epoch(); e != 1 {
		t.Fatalf("first boot epoch = %d, want 1", e)
	}
	name := UniqueSheet(t)
	r := NewReader(t, first, name)
	ctx := t.Context()
	if err := r.SetWithTTL(ctx, "a", 1, 60); err != nil {
		t.Fatal(err)
	}
	if err := r.SetWithTTL(ctx, "b", map[string]int{"n": 2}, 60); err != nil {
		t.Fatal(err)
	}
	if err := r.Set(ctx, "short", 3); err != nil { // ttl 0 → REDLITE_CACHE_TTL 300ms
		t.Fatal(err)
	}
	expiresBy := time.Now().Add(300 * time.Millisecond)
	Eventually(t, "three rows flushed", func() bool { return rowCount(t, first, name) == 3 })
	first.Stop()
	if err := first.Err(); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	time.Sleep(time.Until(expiresBy) + 50*time.Millisecond) // let "short" expire in the table

	second := StartLeader(t, withPersist(dbURL))
	s, ok := second.App.Registry().Get(name)
	if !ok {
		t.Fatal("sheet not hydrated")
	}
	if _, ok := s.Get("a"); !ok {
		t.Fatal("key a not hydrated")
	}
	if e, ok := s.Get("b"); !ok || string(e.Value) != `{"n":2}` {
		t.Fatalf("key b hydrated as %q, %v", e.Value, ok)
	}
	if _, ok := s.Get("short"); ok || s.Len() != 2 {
		t.Fatalf("expired key loaded (len %d)", s.Len())
	}
	if n := rowCount(t, second, name); n != 3 {
		t.Fatalf("rows = %d: the expired row must be skipped by hydration, not deleted first", n)
	}
	if e := second.App.Node().Epoch(); e != 1 {
		t.Fatalf("epoch after restart = %d, want 1", e)
	}
	if resp, _ := Dial(t, second.WSURL).Open(name, "wrong"); resp.Error != protocol.ErrMsgInvalidPassword {
		t.Fatalf("wrong password after restart = %+v", resp)
	}
	r2 := NewReader(t, second, name)
	var a int
	if !r2.Get("a", &a) || a != 1 {
		t.Fatalf("reader Get(a) after restart = %d", a)
	}
}

// TestPersistQueueFullNeverBlocks: with a one-slot queue and one statement per flush, many sets
// complete promptly; the overflow is dropped (counted, Warn), never waited for.
func TestPersistQueueFullNeverBlocks(t *testing.T) {
	dbURL := persistURL(t)
	srv := StartLeader(t, withPersist(dbURL), func(c *config.Config) { c.PersistQueueSize = 1; c.PersistBatchSize = 1 })
	r := NewReader(t, srv, UniqueSheet(t))
	ctx := t.Context()
	start := time.Now()
	for i := range 2000 {
		if err := r.Set(ctx, fmt.Sprintf("k%d", i), i); err != nil {
			t.Fatalf("set %d: %v", i, err)
		}
	}
	if took := time.Since(start); took > Timeout/2 {
		t.Fatalf("2000 sets took %s with a full persist queue", took)
	}
	if srv.App.Writer().Dropped() == 0 {
		t.Fatal("no op was dropped: the queue never filled (test did not exercise backpressure)")
	}
}

// TestPersistEpochAcrossPromotion: a replica sharing the database promotes to epoch 2 and stores
// it; a leader booted afterwards on the same database resumes at 2.
func TestPersistEpochAcrossPromotion(t *testing.T) {
	dbURL := persistURL(t)
	leader := StartLeader(t, withPersist(dbURL))
	name := UniqueSheet(t)
	if err := NewReader(t, leader, name).Set(t.Context(), "k", 1); err != nil {
		t.Fatal(err)
	}
	rep := StartReplica(t, leader, withPersist(dbURL))
	Eventually(t, "replica synced", func() bool { return sameState(leader, rep, name) })
	leader.Stop()
	Eventually(t, "replica promoted", rep.App.Node().IsLeader)
	if e := rep.App.Node().Epoch(); e != 2 {
		t.Fatalf("promoted epoch = %d, want 2", e)
	}
	// The promoted node persists: a write lands in the shared table.
	if err := NewReader(t, rep, name).Set(t.Context(), "after", 2); err != nil {
		t.Fatal(err)
	}
	Eventually(t, "write after promotion persisted", func() bool { return rowCount(t, rep, name) == 2 })
	rep.Stop()

	next := StartLeader(t, withPersist(dbURL))
	if e := next.App.Node().Epoch(); e != 2 {
		t.Fatalf("epoch after promotion + restart = %d, want 2", e)
	}
	if _, ok := next.App.Registry().Get(name); !ok {
		t.Fatal("sheet not hydrated after promotion + restart")
	}
}

func entriesPersistence(t *testing.T, dbURL string) string {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.WithoutCancel(t.Context()))
	var p string
	if err := conn.QueryRow(t.Context(), "SELECT relpersistence::text FROM pg_class WHERE oid = 'cache_entries'::regclass").Scan(&p); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestPersistLoggedFlag (D-020): REDLITE_PERSIST_LOGGED is authoritative at every boot.
func TestPersistLoggedFlag(t *testing.T) {
	dbURL := persistURL(t)
	s := StartLeader(t, withPersist(dbURL), func(c *config.Config) { c.PersistLogged = true })
	s.Stop()
	if p := entriesPersistence(t, dbURL); p != "p" {
		t.Fatalf("relpersistence = %q after LOGGED=true, want p", p)
	}
	s = StartLeader(t, withPersist(dbURL))
	s.Stop()
	if p := entriesPersistence(t, dbURL); p != "u" {
		t.Fatalf("relpersistence = %q after LOGGED=false, want u", p)
	}
}

// TestPersistShutdownFlushesEverything (review HIGH, shutdown ordering): with no flush before
// shutdown (1h interval, huge batch), every acked set is in Postgres after Stop: the writer runs
// until every connection closed, then drains and flushes.
func TestPersistShutdownFlushesEverything(t *testing.T) {
	dbURL := persistURL(t)
	srv := StartLeader(t, withPersist(dbURL), func(c *config.Config) {
		c.PersistFlushInterval = time.Hour
		c.PersistBatchSize = 100000
	})
	name := UniqueSheet(t)
	r := NewReader(t, srv, name)
	for i := range 500 {
		if err := r.Set(t.Context(), fmt.Sprintf("k%d", i), i); err != nil {
			t.Fatal(err)
		}
	}
	srv.Stop()
	conn, err := pgx.Connect(t.Context(), dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.WithoutCancel(t.Context()))
	var n int
	if err := conn.QueryRow(t.Context(), "SELECT count(*) FROM cache_entries WHERE sheet = $1", name).Scan(&n); err != nil || n != 500 {
		t.Fatalf("rows after shutdown = %d (err %v), want 500", n, err)
	}
}
