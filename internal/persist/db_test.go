// Covers: internal/persist
package persist

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/CrimsonCoder42/red_lite_cache/db"
	"github.com/CrimsonCoder42/red_lite_cache/internal/config"
	"github.com/CrimsonCoder42/red_lite_cache/internal/sheet"
)

// testStore migrates a fresh schema on REDLITE_TEST_DATABASE_URL and returns a Store bound to it
// (search_path in the URL). The schema is dropped in cleanup. Skips without the variable.
func testStore(t *testing.T) *db.Store {
	t.Helper()
	base := config.TestDatabaseURL()
	if base == "" {
		t.Skip("SKIPPED: persist (no REDLITE_TEST_DATABASE_URL)")
	}
	ctx := t.Context()
	schema := fmt.Sprintf("rl_test_%d", time.Now().UnixNano())

	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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
	scoped := u.String()

	if err := db.Migrate(scoped); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := db.OpenPool(ctx, scoped, 1, 2)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return db.NewStore(pool)
}

func TestDBWriterHydrateRoundTrip(t *testing.T) {
	store := testStore(t)
	ctx := t.Context()
	w := NewWriter(store, Options{BatchSize: 100, FlushInterval: time.Hour}, nil)
	stop := runWriter(t, w)

	now := time.Now()
	future := now.Add(time.Hour).Truncate(time.Microsecond)
	op := func(o sheet.Op) { o.Sheet, o.ProjectID = "s1", defaultProject; w.Observe(o) }
	op(sheet.Op{Kind: sheet.OpCreate, PasswordHash: []byte("$2a$hash")})
	op(sheet.Op{Kind: sheet.OpSet, Key: "forever", Value: json.RawMessage(`{"a":1}`)})
	op(sheet.Op{Kind: sheet.OpSet, Key: "live", Value: json.RawMessage(`2`), ExpiresAt: future})
	op(sheet.Op{Kind: sheet.OpSet, Key: "dead", Value: json.RawMessage(`3`), ExpiresAt: now.Add(-time.Minute)})
	op(sheet.Op{Kind: sheet.OpSet, Key: "user:1", Value: json.RawMessage(`4`)})
	op(sheet.Op{Kind: sheet.OpDeletePrefix, Prefix: "user:"})
	stop() // shutdown flush

	got := map[string]EntryRow{}
	sheets, n, err := Hydrate(ctx, store, func(s SheetRow, rows []EntryRow) error {
		if s.Name != "s1" || s.ProjectID != defaultProject || string(s.PasswordHash) != "$2a$hash" {
			t.Errorf("sheet row = %+v", s)
		}
		for _, r := range rows {
			got[r.Key] = r
		}
		return nil
	})
	if err != nil {
		t.Fatalf("hydrate: %v", err)
	}
	if sheets != 1 || n != 2 || len(got) != 2 {
		t.Fatalf("hydrated sheets=%d entries=%d %v, want 1 sheet, forever+live", sheets, n, got)
	}
	if !got["forever"].Entry.ExpiresAt.IsZero() || !got["live"].Entry.ExpiresAt.Equal(future) {
		t.Fatalf("expiry not round-tripped: %+v", got)
	}
	if _, ok := got["dead"]; ok {
		t.Fatal("expired row was loaded")
	}

	// The cleaner removes the expired row that hydration skipped.
	cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	go func() { _ = RunCleaner(cctx, store, 20*time.Millisecond) }()
	waitFor(t, "cleaner", func() bool {
		var c int
		if err := store.Pool().QueryRow(ctx, "SELECT count(*) FROM cache_entries WHERE key = 'dead'").Scan(&c); err != nil {
			t.Fatalf("count: %v", err)
		}
		return c == 0
	})
}

func TestDBEpochMonotonic(t *testing.T) {
	store := testStore(t)
	ctx := t.Context()
	if e, err := LoadEpoch(ctx, store); err != nil || e != 0 {
		t.Fatalf("LoadEpoch on empty = %d, %v; want 0, nil", e, err)
	}
	if e, err := RaiseEpoch(ctx, store, 5, "a"); err != nil || e != 5 {
		t.Fatalf("raise 5 = %d, %v", e, err)
	}
	if e, err := RaiseEpoch(ctx, store, 3, "b"); err != nil || e != 5 {
		t.Fatalf("raise 3 after 5 = %d, %v; want 5", e, err)
	}
	if e, err := LoadEpoch(ctx, store); err != nil || e != 5 {
		t.Fatalf("LoadEpoch = %d, %v; want 5", e, err)
	}
}

// A writer whose node epoch is below cluster_state's writes nothing (fenced leader, shared DB).
func TestDBFlushRefusedForStaleEpoch(t *testing.T) {
	store := testStore(t)
	ctx := t.Context()
	if _, err := RaiseEpoch(ctx, store, 5, "newer"); err != nil {
		t.Fatal(err)
	}
	w := NewWriter(store, Options{BatchSize: 100, FlushInterval: time.Hour, Epoch: func() uint64 { return 4 }}, nil)
	stop := runWriter(t, w)
	w.Observe(sheet.Op{Kind: sheet.OpCreate, Sheet: "fenced", ProjectID: defaultProject, PasswordHash: []byte("h")})
	stop()
	if w.Refused() != 1 {
		t.Fatalf("refused = %d, want 1", w.Refused())
	}
	var n int
	if err := store.Pool().QueryRow(ctx, "SELECT count(*) FROM sheets WHERE name = 'fenced'").Scan(&n); err != nil || n != 0 {
		t.Fatalf("fenced writer wrote %d rows (err %v)", n, err)
	}
	w2 := NewWriter(store, Options{BatchSize: 100, FlushInterval: time.Hour, Epoch: func() uint64 { return 5 }}, nil)
	stop = runWriter(t, w2)
	w2.Observe(sheet.Op{Kind: sheet.OpCreate, Sheet: "current", ProjectID: defaultProject, PasswordHash: []byte("h")})
	stop()
	if w2.Refused() != 0 {
		t.Fatal("current-epoch writer refused")
	}
}
