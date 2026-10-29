// Covers: internal/persist
package persist

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/CrimsonCoder42/red_lite_cache/internal/logger"
	"github.com/CrimsonCoder42/red_lite_cache/internal/sheet"
)

// syncBuf is a goroutine-safe log sink.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// captureLog points logger.L at a buffer for the test.
func captureLog(t *testing.T) *syncBuf {
	t.Helper()
	buf := &syncBuf{}
	prev := logger.L
	logger.L = logger.New(buf, slog.LevelDebug, false)
	t.Cleanup(func() { logger.L = prev })
	return buf
}

var testProject = uuid.MustParse("11111111-1111-1111-1111-111111111111")

func set(key, v string) sheet.Op {
	return sheet.Op{Kind: sheet.OpSet, Sheet: "s", ProjectID: testProject, Key: key, Value: json.RawMessage(v)}
}

func del(key string) sheet.Op {
	return sheet.Op{Kind: sheet.OpDelete, Sheet: "s", ProjectID: testProject, Key: key}
}

// describe renders a plan as "kind:match[=value]" so a test compares one string slice.
func describe(stmts []stmt) []string {
	out := make([]string, len(stmts))
	for i, s := range stmts {
		out[i] = s.kind.String() + ":" + s.match
		if s.kind == stmtUpsertEntry {
			out[i] += "=" + string(s.value)
		}
		if s.kind == stmtUpsertSheet {
			out[i] = s.kind.String() + ":" + s.sheet
		}
		if s.kind == stmtDeleteEntries {
			out[i] = s.kind.String() + ":" + strings.Join(s.keys, ",")
		}
	}
	return out
}

func TestPlanCollapse(t *testing.T) {
	exp := time.Now().Add(time.Hour)
	cases := []struct {
		name string
		ops  []sheet.Op
		want []string
	}{
		{"set set keeps last", []sheet.Op{set("k1", "1"), set("k1", "2")},
			[]string{"upsert_entry:k1=2"}},
		{"set then delete keeps delete", []sheet.Op{set("k1", "1"), del("k1")},
			[]string{"delete_entry:k1"}},
		{"delete then set keeps set", []sheet.Op{del("k1"), set("k1", "1")},
			[]string{"upsert_entry:k1=1"}},
		{"prefix delete cancels covered sets, later set survives", []sheet.Op{
			set("user:1", "1"), set("other", "o"),
			{Kind: sheet.OpDeletePrefix, Sheet: "s", ProjectID: testProject, Prefix: "user:"},
			set("user:1", "2"),
		}, []string{"upsert_entry:other=o", "delete_prefix:user:", "upsert_entry:user:1=2"}},
		{"suffix delete cancels covered sets", []sheet.Op{
			set("a:tok", "1"), set("b:tok", "2"), set("b:x", "3"),
			{Kind: sheet.OpDeleteSuffix, Sheet: "s", ProjectID: testProject, Suffix: ":tok"},
		}, []string{"upsert_entry:b:x=3", "delete_suffix::tok"}},
		{"prefix delete does not touch another sheet", []sheet.Op{
			{Kind: sheet.OpSet, Sheet: "other", ProjectID: testProject, Key: "user:1", Value: json.RawMessage("9")},
			{Kind: sheet.OpDeletePrefix, Sheet: "s", ProjectID: testProject, Prefix: "user:"},
		}, []string{"upsert_entry:user:1=9", "delete_prefix:user:"}},
		{"empty prefix covers every key", []sheet.Op{
			set("a", "1"), set("b", "2"),
			{Kind: sheet.OpDeletePrefix, Sheet: "s", ProjectID: testProject, Prefix: ""},
		}, []string{"delete_prefix:"}},
		{"expire is one multi-key delete and cancels its earlier set", []sheet.Op{
			{Kind: sheet.OpSet, Sheet: "s", ProjectID: testProject, Key: "k1", Value: json.RawMessage("1"), ExpiresAt: exp},
			{Kind: sheet.OpExpire, Sheet: "s", ProjectID: testProject, Keys: []string{"k1", "k2"}},
		}, []string{"delete_entries:k1,k2"}},
		{"set after expire survives after it", []sheet.Op{
			{Kind: sheet.OpExpire, Sheet: "s", ProjectID: testProject, Keys: []string{"k1", "k2"}},
			set("k1", "2"),
			{Kind: sheet.OpDeletePrefix, Sheet: "s", ProjectID: testProject, Prefix: "k2"},
		}, []string{"delete_entries:k1,k2", "upsert_entry:k1=2", "delete_prefix:k2"}},
		{"create stays first and is never collapsed", []sheet.Op{
			{Kind: sheet.OpCreate, Sheet: "s", ProjectID: testProject, PasswordHash: []byte("h")},
			set("k1", "1"),
			{Kind: sheet.OpDeletePrefix, Sheet: "s", ProjectID: testProject, Prefix: ""},
		}, []string{"upsert_sheet:s", "delete_prefix:"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := describe(plan(c.ops))
			if strings.Join(got, " | ") != strings.Join(c.want, " | ") {
				t.Fatalf("plan:\n got  %q\n want %q", got, c.want)
			}
		})
	}
}

func TestPlanMapsCreateAndExpiry(t *testing.T) {
	exp := time.Now().Add(time.Minute)
	got := plan([]sheet.Op{
		{Kind: sheet.OpCreate, Sheet: "auto", ProjectID: defaultProject, PasswordHash: []byte("h")},
		{Kind: sheet.OpCreate, Sheet: "admin", ProjectID: testProject, PasswordHash: []byte("h")},
		{Kind: sheet.OpSet, Sheet: "auto", ProjectID: defaultProject, Key: "k", Value: json.RawMessage("1"), ExpiresAt: exp},
	})
	if len(got) != 3 || !got[0].autocreated || got[1].autocreated {
		t.Fatalf("autocreated must be true only for the default project: %+v", got)
	}
	if !got[2].expiresAt.Equal(exp) || got[2].project != defaultProject {
		t.Fatalf("set lost expiry or project: %+v", got[2])
	}
}

// newTestWriter builds a writer whose flush records each plan instead of touching a database.
func newTestWriter(o Options, active func() bool, fail func(n int) error) (*Writer, func() [][]stmt) {
	w := NewWriter(nil, o, active)
	var mu sync.Mutex
	var flushes [][]stmt
	w.flush = func(_ context.Context, stmts []stmt, _ uint64) error {
		mu.Lock()
		defer mu.Unlock()
		if fail != nil {
			if err := fail(len(flushes)); err != nil {
				flushes = append(flushes, nil) // record the attempt
				return err
			}
		}
		flushes = append(flushes, append([]stmt(nil), stmts...))
		return nil
	}
	return w, func() [][]stmt { mu.Lock(); defer mu.Unlock(); return append([][]stmt(nil), flushes...) }
}

func TestObserveNeverBlocksWhenFull(t *testing.T) {
	logs := captureLog(t)
	w, _ := newTestWriter(Options{QueueSize: 2}, nil, nil)
	w.Observe(set("a", "1"))
	w.Observe(set("b", "1"))

	done := make(chan struct{})
	go func() {
		for range 1000 {
			w.Observe(set("c", "1"))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Observe blocked on a full queue")
	}
	if got := w.Dropped(); got != 1000 {
		t.Fatalf("dropped = %d, want 1000", got)
	}
	if n := strings.Count(logs.String(), "Persist: enqueue dropped"); n != 1 {
		t.Fatalf("enqueue dropped Warn lines = %d, want 1 (rate-limited)\n%s", n, logs.String())
	}
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "cap=2") {
		t.Fatalf("Warn missing level or cap:\n%s", logs.String())
	}
}

func TestInactiveWriterIgnoresOps(t *testing.T) {
	w, _ := newTestWriter(Options{QueueSize: 1}, func() bool { return false }, nil)
	for range 5 {
		w.Observe(set("a", "1"))
	}
	if len(w.ops) != 0 || w.Dropped() != 0 {
		t.Fatalf("inactive writer queued %d / dropped %d", len(w.ops), w.Dropped())
	}
}

func runWriter(t *testing.T, w *Writer) (cancel func()) {
	t.Helper()
	ctx, stop := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	return func() {
		stop()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Run returned %v, want nil", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Run did not return after cancel")
		}
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRunFlushesOnSize(t *testing.T) {
	w, flushes := newTestWriter(Options{BatchSize: 3, FlushInterval: time.Hour}, nil, nil)
	stop := runWriter(t, w)
	defer stop()
	w.Observe(set("a", "1"))
	w.Observe(set("b", "1"))
	w.Observe(set("c", "1"))
	waitFor(t, "size flush", func() bool { return len(flushes()) == 1 })
	if got := len(flushes()[0]); got != 3 {
		t.Fatalf("flushed %d statements, want 3", got)
	}
}

func TestRunFlushesOnInterval(t *testing.T) {
	w, flushes := newTestWriter(Options{BatchSize: 100, FlushInterval: 20 * time.Millisecond}, nil, nil)
	stop := runWriter(t, w)
	defer stop()
	w.Observe(set("a", "1"))
	waitFor(t, "interval flush", func() bool { return len(flushes()) == 1 })
}

func TestRunFlushesOnCancel(t *testing.T) {
	w, flushes := newTestWriter(Options{BatchSize: 100, FlushInterval: time.Hour}, nil, nil)
	// Queued before Run starts: the shutdown path must drain the channel, not only the batch.
	w.Observe(set("a", "1"))
	w.Observe(set("b", "1"))
	runWriter(t, w)()
	got := flushes()
	if len(got) != 1 || len(got[0]) != 2 {
		t.Fatalf("shutdown flush = %v, want one flush of 2", got)
	}
}

func TestRunRetriesOnceThenDrops(t *testing.T) {
	logs := captureLog(t)
	boom := errors.New("db down")
	// Attempts 0 and 1 fail (first try + its one retry); every later attempt succeeds.
	w, flushes := newTestWriter(Options{BatchSize: 1, FlushInterval: 20 * time.Millisecond}, nil,
		func(n int) error {
			if n < 2 {
				return boom
			}
			return nil
		})
	stop := runWriter(t, w)
	w.Observe(set("lost", "1")) // flush fails → held
	waitFor(t, "first failure", func() bool { return len(flushes()) == 1 })
	w.Observe(set("kept", "1")) // retry of "lost" fails → dropped; "kept" then succeeds
	waitFor(t, "retry and next flush", func() bool { return len(flushes()) == 3 })
	stop()

	got := flushes()
	if got[2] == nil || describe(got[2])[0] != "upsert_entry:kept=1" {
		t.Fatalf("third flush = %v, want kept", describe(got[2]))
	}
	out := logs.String()
	if !strings.Contains(out, "Persist: flush failed") || !strings.Contains(out, "Persist: batch dropped") {
		t.Fatalf("missing flush failed / batch dropped lines:\n%s", out)
	}
	if strings.Count(out, "Persist: batch dropped") != 1 {
		t.Fatalf("batch dropped logged more than once:\n%s", out)
	}
}

// A create that meets a full queue is parked, not dropped, and its next flush upserts the sheet
// row before the sheet's entries (the FK poisoning fix).
func TestCreateParkedOnFullQueue(t *testing.T) {
	w, flushes := newTestWriter(Options{QueueSize: 1, BatchSize: 100, FlushInterval: 20 * time.Millisecond}, nil, nil)
	w.Observe(set("filler", "1")) // fills the queue
	w.Observe(sheet.Op{Kind: sheet.OpCreate, Sheet: "s", ProjectID: testProject, PasswordHash: []byte("h")})
	if w.Dropped() != 0 {
		t.Fatalf("create was dropped (dropped=%d)", w.Dropped())
	}
	stop := runWriter(t, w)
	w.Observe(set("k", "1"))
	waitFor(t, "flush with the parked create", func() bool {
		for _, f := range flushes() {
			for _, d := range describe(f) {
				if d == "upsert_sheet:s" {
					return true
				}
			}
		}
		return false
	})
	stop()
	all := []string{}
	for _, f := range flushes() {
		all = append(all, describe(f)...)
	}
	if all[0] != "upsert_sheet:s" {
		t.Fatalf("parked create must flush before every entry: %q", all)
	}
}

// A batch dropped after its retry parks its creates again, so the sheet row still lands later.
func TestDroppedBatchReparksCreate(t *testing.T) {
	captureLog(t)
	boom := errors.New("db down")
	w, flushes := newTestWriter(Options{BatchSize: 100, FlushInterval: 20 * time.Millisecond}, nil,
		func(n int) error {
			if n < 2 {
				return boom
			}
			return nil
		})
	stop := runWriter(t, w)
	w.Observe(sheet.Op{Kind: sheet.OpCreate, Sheet: "s", ProjectID: testProject, PasswordHash: []byte("h")})
	w.Observe(set("k", "1"))
	waitFor(t, "create flushed after the drop", func() bool {
		got := flushes()
		return len(got) >= 3 && got[2] != nil && describe(got[2])[0] == "upsert_sheet:s"
	})
	stop()
}

func TestEnsureSheetFlushesCreate(t *testing.T) {
	w, flushes := newTestWriter(Options{BatchSize: 100, FlushInterval: 20 * time.Millisecond}, nil, nil)
	stop := runWriter(t, w)
	w.EnsureSheet(sheet.Op{Sheet: "promoted", ProjectID: testProject, PasswordHash: []byte("h")})
	waitFor(t, "ensured sheet flushed", func() bool {
		got := flushes()
		return len(got) == 1 && describe(got[0])[0] == "upsert_sheet:promoted"
	})
	stop()
}

// Review HIGH (fenced leader): a flush refused for a stale epoch is dropped at once, never retried,
// and logged at Error; the node's epoch reaches the flush seam.
func TestStaleEpochFlushRefusedNotRetried(t *testing.T) {
	logs := captureLog(t)
	var mu sync.Mutex
	var attempts int
	var seen []uint64
	w := NewWriter(nil, Options{BatchSize: 1, FlushInterval: 20 * time.Millisecond, Epoch: func() uint64 { return 3 }}, nil)
	w.flush = func(_ context.Context, _ []stmt, epoch uint64) error {
		mu.Lock()
		defer mu.Unlock()
		attempts++
		seen = append(seen, epoch)
		return checkEpoch(4, epoch) // a newer leader stored epoch 4
	}
	stop := runWriter(t, w)
	w.Observe(set("k", "1"))
	waitFor(t, "refused", func() bool { return w.Refused() == 1 })
	time.Sleep(100 * time.Millisecond) // several ticks: a retry would show here
	stop()
	mu.Lock()
	defer mu.Unlock()
	if attempts != 1 || seen[0] != 3 {
		t.Fatalf("attempts = %d epochs %v, want one attempt at epoch 3", attempts, seen)
	}
	if !strings.Contains(logs.String(), "Persist: flush refused, stale epoch") || !strings.Contains(logs.String(), "level=ERROR") {
		t.Fatalf("missing Error line:\n%s", logs.String())
	}
}

// TestFlushFencesOnAcceptanceTerm (second review, HIGH): ops accepted at epoch 1, then the node
// steps down and adopts epoch 2 before the flush. The flush must fence on term 1, not on the
// node's epoch at flush time, or the old term's writes overwrite the new leader's rows.
func TestFlushFencesOnAcceptanceTerm(t *testing.T) {
	var epoch atomic.Uint64
	epoch.Store(1)
	var mu sync.Mutex
	var seen []uint64
	w := NewWriter(nil, Options{BatchSize: 100, FlushInterval: time.Hour, Epoch: epoch.Load}, nil)
	w.flush = func(_ context.Context, _ []stmt, e uint64) error {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, e)
		return checkEpoch(2, e) // the new leader stored epoch 2
	}
	w.Observe(set("k", "old-term"))
	epoch.Store(2) // step-down adopts the newer epoch
	stop := runWriter(t, w)
	stop() // drains and flushes on cancel
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 || seen[0] != 1 || w.Refused() != 1 {
		t.Fatalf("flush epochs %v refused %d, want one flush fenced at term 1 and refused", seen, w.Refused())
	}
}

func TestCheckEpoch(t *testing.T) {
	if err := checkEpoch(2, 2); err != nil {
		t.Fatalf("equal epoch refused: %v", err)
	}
	if err := checkEpoch(0, 1); err != nil {
		t.Fatalf("no stored epoch refused: %v", err)
	}
	if err := checkEpoch(3, 2); !errors.Is(err, ErrStaleEpoch) {
		t.Fatalf("stored 3 > mine 2 = %v, want ErrStaleEpoch", err)
	}
}

// Review HIGH (shutdown ordering): everything enqueued before Run's ctx is cancelled is flushed,
// however many batches it spans.
func TestRunDrainsWholeQueueOnCancel(t *testing.T) {
	w, flushes := newTestWriter(Options{BatchSize: 10, FlushInterval: time.Hour, QueueSize: 5000}, nil, nil)
	for i := range 3000 {
		w.Observe(set(fmt.Sprintf("k%d", i), "1"))
	}
	runWriter(t, w)() // start, cancel at once, wait
	n := 0
	for _, f := range flushes() {
		n += len(f)
	}
	if n != 3000 || w.Dropped() != 0 {
		t.Fatalf("flushed %d statements (dropped %d), want 3000", n, w.Dropped())
	}
}

func flushedSheets(flushes [][]stmt) map[string]int {
	out := map[string]int{}
	for _, f := range flushes {
		for _, s := range f {
			out[s.sheet]++
		}
	}
	return out
}

// A revoked project's queued create + set and its parked create never reach the flush; another
// sheet's ops in the same batch do, and an op accepted after the revoke (a re-created sheet) does.
func TestRevokeDiscardsQueuedAndParkedOps(t *testing.T) {
	captureLog(t)
	w, flushes := newTestWriter(Options{QueueSize: 3, BatchSize: 100, FlushInterval: 20 * time.Millisecond}, nil, nil)
	w.Observe(sheet.Op{Kind: sheet.OpCreate, Sheet: "s", ProjectID: testProject, PasswordHash: []byte("h")})
	w.Observe(sheet.Op{Kind: sheet.OpSet, Sheet: "s", ProjectID: testProject, Key: "k", Value: json.RawMessage("1")})
	w.Observe(sheet.Op{Kind: sheet.OpSet, Sheet: "other", ProjectID: defaultProject, Key: "k", Value: json.RawMessage("2")})
	// Queue full: this create of another sheet of the project is parked.
	w.Observe(sheet.Op{Kind: sheet.OpCreate, Sheet: "s2", ProjectID: testProject, PasswordHash: []byte("h")})

	w.RevokeProject(testProject, []string{"s"})
	stop := runWriter(t, w)
	waitFor(t, "other flushed", func() bool { return flushedSheets(flushes())["other"] == 1 })
	if got := flushedSheets(flushes()); got["s"] != 0 || got["s2"] != 0 {
		t.Fatalf("revoked sheets reached the flush: %v", got)
	}
	// Re-created after the revoke: persisted normally (the revocation does not cover it).
	w.Observe(sheet.Op{Kind: sheet.OpCreate, Sheet: "s", ProjectID: testProject, PasswordHash: []byte("h2")})
	waitFor(t, "re-created sheet flushed", func() bool { return flushedSheets(flushes())["s"] == 1 })
	stop()
	w.revMu.Lock()
	defer w.revMu.Unlock()
	if len(w.revs) != 0 {
		t.Fatalf("revocations not pruned after an empty flush: %d", len(w.revs))
	}
}

// A held (failed) batch whose sheet is revoked before the retry is not retried for that sheet, and
// its create is not parked again.
func TestRevokeDiscardsHeldBatch(t *testing.T) {
	captureLog(t)
	boom := errors.New("db down")
	w, flushes := newTestWriter(Options{BatchSize: 100, FlushInterval: 20 * time.Millisecond}, nil,
		func(n int) error {
			if n == 0 {
				return boom
			}
			return nil
		})
	stop := runWriter(t, w)
	w.Observe(sheet.Op{Kind: sheet.OpCreate, Sheet: "s", ProjectID: testProject, PasswordHash: []byte("h")})
	w.Observe(set("k", "1"))
	waitFor(t, "first attempt failed", func() bool { return len(flushes()) == 1 })
	w.RevokeSheets([]string{"s"})
	time.Sleep(100 * time.Millisecond) // several ticks
	stop()
	if got := flushedSheets(flushes()); got["s"] != 0 {
		t.Fatalf("revoked sheet retried: %v", got)
	}
}
