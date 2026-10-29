package persist

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/CrimsonCoder42/red_lite_cache/db"
	"github.com/CrimsonCoder42/red_lite_cache/db/sqlc/entries"
	"github.com/CrimsonCoder42/red_lite_cache/db/sqlc/registry"
	"github.com/CrimsonCoder42/red_lite_cache/internal/logger"
	"github.com/CrimsonCoder42/red_lite_cache/internal/sheet"
)

// Defaults used when an Options field is zero or negative (doc/persist/design.md, internal/config).
const (
	defaultBatchSize       = 100
	defaultFlushInterval   = 5 * time.Second
	defaultQueueSize       = 10000
	defaultCleanupInterval = time.Minute

	// shutdownFlushTimeout bounds the final flush after ctx is cancelled, and every flush.
	shutdownFlushTimeout = 10 * time.Second
	// dropWarnEvery is the minimum gap between two "Persist: enqueue dropped" lines.
	dropWarnEvery = time.Second
)

// Options sizes the writer. Zero fields take the defaults above.
type Options struct {
	BatchSize       int
	FlushInterval   time.Duration
	QueueSize       int
	CleanupInterval time.Duration // read by the caller for RunCleaner; kept here so config maps 1:1
	// Epoch is this node's leader epoch (replica.Node.Epoch), read when an op is accepted: each op
	// carries the term it was accepted in. Every flush transaction compares the batch's OLDEST term
	// with cluster_state.epoch and refuses the batch when the stored epoch is higher, so ops a
	// fenced leader accepted before it stepped down (and adopted the newer epoch) never land after a
	// newer leader exists. nil = no check.
	Epoch func() uint64
}

// ErrStaleEpoch is a flush refused because cluster_state holds a higher epoch than this node's.
var ErrStaleEpoch = errors.New("persist: stale epoch")

func (o Options) withDefaults() Options {
	if o.BatchSize <= 0 {
		o.BatchSize = defaultBatchSize
	}
	if o.FlushInterval <= 0 {
		o.FlushInterval = defaultFlushInterval
	}
	if o.QueueSize <= 0 {
		o.QueueSize = defaultQueueSize
	}
	if o.CleanupInterval <= 0 {
		o.CleanupInterval = defaultCleanupInterval
	}
	return o
}

// Writer is the write-behind sheet.Observer: Observe enqueues without blocking, Run is the one
// goroutine that batches ops into transactions.
// after code-heim go_18_worker_pool: one worker, ctx-cancelled, flush on size or interval; the
// pool of N workers became one so per-sheet op order survives into the database.
// after advo_cache/CACHE_PERSISTENCE_REPORT.md:506-529 (Writer.worker): same select on op / ticker,
// plus a non-blocking enqueue, per-key collapse, one retry, and a ctx-bounded final flush.
type Writer struct {
	o      Options
	active func() bool
	ops    chan queued
	// flush runs one planned batch in one transaction. NewWriter sets it to the database flush;
	// unit tests replace it so the batching logic runs without Postgres.
	// epoch is the node's epoch at flush time (0 = no check).
	flush func(ctx context.Context, stmts []stmt, epoch uint64) error

	dropped     atomic.Uint64 // every op dropped since start
	refused     atomic.Uint64 // batches refused for a stale epoch
	unreported  atomic.Uint64 // dropped since the last Warn
	lastDropLog atomic.Int64  // unix nanos of the last Warn

	// parked holds sheet creates that could not be queued (full queue, or EnsureSheet), keyed by
	// sheet name, latest wins. A create is never dropped: every entry row references its sheet row
	// (FK), so one lost create would fail every later batch that touches the sheet. The writer
	// prepends them to its next flush. Bounded by the number of sheets.
	parkMu  sync.Mutex
	parked  map[string]queued
	hasPark atomic.Bool

	// seq stamps every accepted op (queued.n). A revocation records seq at revoke time (mark) and
	// discards every op of its project/sheets stamped at or before it, so a revoked sheet never comes
	// back in Postgres from ops still queued, held or parked (a restart would hydrate it with its
	// old password). Revokes are rare; a revocation is pruned once the queue and the held batch are
	// empty after a flush (no op it covers can still be inside the writer).
	seq atomic.Uint64
	// flushMu is held for one flush (filter + transaction). revoke takes it once after recording
	// its revocation, as a barrier: a flush that started before the revoke has committed, every
	// later flush sees the revocation. It is the one lock held across database IO, by design.
	flushMu sync.Mutex
	revMu   sync.Mutex
	revs    []revocation
	hasRev  atomic.Bool
}

// queued is one accepted op with its acceptance stamp and the leader term (epoch) it was accepted
// in. The term is what the flush fences on: a node that stepped down adopts the newer epoch, so
// comparing against the node's epoch at flush time would let an old term's ops through.
type queued struct {
	op    sheet.Op
	n     uint64
	epoch uint64 // 0 = no fencing (Options.Epoch nil)
}

// revocation is one revoked project (when hasProject) and/or set of sheet names, covering every op
// stamped at or before mark.
type revocation struct {
	project    uuid.UUID
	hasProject bool
	sheets     map[string]bool
	mark       uint64
}

func (r revocation) covers(q queued) bool {
	return q.n <= r.mark && ((r.hasProject && q.op.ProjectID == r.project) || r.sheets[q.op.Sheet])
}

// RevokeProject discards every queued, held and parked op of project or of the named sheets that
// the writer accepted before this call. Call it after the sheets were dropped from the registry
// (sheet.Registry.Drop), so no later op of theirs predates the mark, and before the sheets are
// deleted in Postgres: it returns only after any flush already in flight has committed, so no
// flush can land after that delete. Blocks at most one flush (≤ 10s).
func (w *Writer) RevokeProject(project uuid.UUID, sheets []string) {
	w.revoke(revocation{project: project, hasProject: true, sheets: nameSet(sheets)})
}

// RevokeSheets is RevokeProject for sheet names only (the cleaner-tick reconcile knows no project).
func (w *Writer) RevokeSheets(sheets []string) {
	w.revoke(revocation{sheets: nameSet(sheets)})
}

func nameSet(names []string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

func (w *Writer) revoke(r revocation) {
	r.mark = w.seq.Load()
	w.revMu.Lock()
	w.revs = append(w.revs, r)
	w.revMu.Unlock()
	w.hasRev.Store(true)
	w.flushMu.Lock() // wait out an in-flight flush (see flushMu)
	w.flushMu.Unlock()
	w.parkMu.Lock()
	for name, q := range w.parked {
		if r.covers(q) {
			delete(w.parked, name)
		}
	}
	w.parkMu.Unlock()
	logger.L.Info("Persist: revoked ops discarded", "project", r.project.String(), "sheets", len(r.sheets), "mark", r.mark)
}

// dropRevoked removes the ops a revocation covers.
func (w *Writer) dropRevoked(qs []queued) []queued {
	if !w.hasRev.Load() {
		return qs
	}
	w.revMu.Lock()
	defer w.revMu.Unlock()
	kept := qs[:0]
	for _, q := range qs {
		if !slices.ContainsFunc(w.revs, func(r revocation) bool { return r.covers(q) }) {
			kept = append(kept, q)
		}
	}
	return kept
}

// pruneRevocations forgets every revocation once nothing it covers can be inside the writer: it
// runs right after a flush (batch empty) with no held batch and an empty queue. Every op of a
// revoked sheet was accepted before its revocation (the sheet was dropped first), and parked ones
// were discarded at revoke time, so none is left.
func (w *Writer) pruneRevocations(heldEmpty bool) {
	if !w.hasRev.Load() || !heldEmpty || len(w.ops) > 0 {
		return
	}
	w.revMu.Lock()
	w.revs = nil
	w.revMu.Unlock()
	w.hasRev.Store(false)
}

// NewWriter builds a writer on store. active reports whether this process persists (only the
// leader does; a replica shares the database); nil means always.
func NewWriter(store *db.Store, o Options, active func() bool) *Writer {
	o = o.withDefaults()
	if active == nil {
		active = func() bool { return true }
	}
	w := &Writer{o: o, active: active, ops: make(chan queued, o.QueueSize), parked: map[string]queued{}}
	w.flush = func(ctx context.Context, stmts []stmt, epoch uint64) error { return execPlan(ctx, store, stmts, epoch) }
	return w
}

// Refused is the number of batches refused because a newer leader's epoch was stored.
func (w *Writer) Refused() uint64 { return w.refused.Load() }

// Observe enqueues op for the writer. It never blocks: it runs under the sheet's mutex. A
// replica (!active()) ignores it; a full queue drops it with a Warn at most once per second
// carrying the count dropped since the last Warn. A sheet create is never dropped: a full queue
// parks it for the next flush (see parked).
func (w *Writer) Observe(op sheet.Op) {
	if !w.active() {
		return
	}
	n := w.seq.Add(1) // stamped before the send: a revocation's mark covers every op accepted so far
	var term uint64
	if w.o.Epoch != nil {
		term = w.o.Epoch() // the term this op was accepted in (we are leader: active() above)
	}
	q := queued{op: op, n: n, epoch: term}
	select {
	case w.ops <- q:
		return
	default:
	}
	if op.Kind == sheet.OpCreate {
		w.park(q)
		return
	}
	w.dropped.Add(1)
	w.unreported.Add(1)
	now := time.Now().UnixNano()
	last := w.lastDropLog.Load()
	if now-last < int64(dropWarnEvery) || !w.lastDropLog.CompareAndSwap(last, now) {
		return
	}
	logger.L.Warn("Persist: enqueue dropped",
		"sheet", op.Sheet, "op", op.Kind.String(),
		"queue_len", len(w.ops), "cap", cap(w.ops), "dropped", w.unreported.Swap(0))
}

// Dropped is the number of ops Observe has dropped since the writer was built.
func (w *Writer) Dropped() uint64 { return w.dropped.Load() }

// EnsureSheet parks a sheet create (op.Kind OpCreate, with Sheet, ProjectID and PasswordHash) so
// the next flush upserts the sheet row before any entry of it. A node that became leader calls it
// for every sheet it holds: those sheets reached it by snapshot, which emits no OpCreate, and a
// missing sheet row would fail every batch that touches the sheet (FK). Never blocks.
func (w *Writer) EnsureSheet(op sheet.Op) {
	op.Kind = sheet.OpCreate
	w.park(queued{op: op, n: w.seq.Add(1)})
}

func (w *Writer) park(q queued) {
	w.parkMu.Lock()
	w.parked[q.op.Sheet] = q
	w.parkMu.Unlock()
	w.hasPark.Store(true)
	logger.L.Debug("Persist: create parked", "sheet", q.op.Sheet)
}

// takeParked returns and clears the parked creates, in sheet-name order (deterministic).
func (w *Writer) takeParked() []queued {
	if !w.hasPark.Load() {
		return nil
	}
	w.parkMu.Lock()
	defer w.parkMu.Unlock()
	out := make([]queued, 0, len(w.parked))
	for _, q := range w.parked {
		out = append(out, q)
	}
	clear(w.parked)
	w.hasPark.Store(false)
	slices.SortFunc(out, func(a, b queued) int { return strings.Compare(a.op.Sheet, b.op.Sheet) })
	return out
}

// reparkCreates parks again every create of a batch that is being dropped: creates are
// idempotent upserts, and losing one would poison every later batch of its sheet. A revoked
// sheet's create is not parked again.
func (w *Writer) reparkCreates(qs []queued) {
	for _, q := range w.dropRevoked(qs) {
		if q.op.Kind == sheet.OpCreate {
			w.park(q)
		}
	}
}

// Run is the one writer goroutine. It batches until BatchSize ops or FlushInterval, collapses
// the batch (plan) and flushes it in one transaction. A failed batch is held and retried once at
// the next flush, before the newer batch so order is kept; a second failure drops it. On ctx.Done
// it drains the queue, flushes everything it holds with its own 10s deadline, and returns nil.
func (w *Writer) Run(ctx context.Context) error {
	ticker := time.NewTicker(w.o.FlushInterval)
	defer ticker.Stop()

	var batch []queued
	var held []queued // one failed batch awaiting its single retry

	flushAll := func(ctx context.Context, final bool) {
		// Parked creates go first, ahead of every entry op of their sheet (FK).
		if parked := w.takeParked(); len(parked) > 0 {
			if len(held) > 0 {
				held = append(parked, held...)
			} else {
				batch = append(parked, batch...)
			}
		}
		if len(held) > 0 {
			if err := w.flushOps(ctx, held); err != nil {
				logger.L.Error("Persist: batch dropped", "ops", len(held), "err", err)
				w.reparkCreates(held)
			}
			held = nil
		}
		if len(batch) > 0 {
			if err := w.flushOps(ctx, batch); err != nil {
				if final { // the shutdown flush has no next tick to retry on
					logger.L.Error("Persist: batch dropped", "ops", len(batch), "err", err)
				} else {
					logger.L.Error("Persist: flush failed", "ops", len(batch), "retry", true, "err", err)
					held = batch
				}
			}
			batch = nil
		}
		w.pruneRevocations(len(held) == 0)
	}

	for {
		select {
		case <-ctx.Done():
			// Bounded by cap so an observer still enqueueing cannot keep shutdown here forever.
			for i, drained := 0, false; i < cap(w.ops) && !drained; i++ {
				select {
				case op := <-w.ops:
					batch = append(batch, op)
				default:
					drained = true
				}
			}
			final, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownFlushTimeout)
			flushAll(final, true)
			cancel()
			return nil
		case op := <-w.ops:
			batch = append(batch, op)
			if len(batch) >= w.o.BatchSize {
				w.flushBounded(ctx, flushAll)
			}
		case <-ticker.C:
			if len(batch) > 0 || len(held) > 0 || w.hasPark.Load() {
				w.flushBounded(ctx, flushAll)
			}
		}
	}
}

// flushBounded runs fn with a context that ends at ctx or after shutdownFlushTimeout, so a hung
// database never stalls the writer longer than that.
func (w *Writer) flushBounded(ctx context.Context, fn func(context.Context, bool)) {
	fctx, cancel := context.WithTimeout(ctx, shutdownFlushTimeout)
	defer cancel()
	fn(fctx, false)
}

// flushOps drops revoked ops, plans the rest and runs the plan through w.flush.
func (w *Writer) flushOps(ctx context.Context, qs []queued) error {
	start := time.Now()
	w.flushMu.Lock()
	defer w.flushMu.Unlock()
	qs = w.dropRevoked(qs)
	ops := make([]sheet.Op, len(qs))
	for i, q := range qs {
		ops[i] = q.op
	}
	stmts := plan(ops)
	if len(stmts) == 0 {
		return nil
	}
	// Fence on the OLDEST term in the batch: one op accepted before a step-down makes the whole
	// batch stale once a newer leader has stored its epoch.
	epoch := oldestTerm(qs)
	if err := w.flush(ctx, stmts, epoch); err != nil {
		if errors.Is(err, ErrStaleEpoch) {
			// Never retried: a newer leader owns the table; this node's view is discarded (D-012).
			w.refused.Add(1)
			logger.L.Error("Persist: flush refused, stale epoch", "ops", len(ops), "epoch", epoch, "err", err)
			return nil
		}
		return err
	}
	// Debug, not Info: at the default 5s interval an Info line per flush is noise.
	logger.L.Debug("Persist: flush completed",
		"ops", len(ops), "statements", len(stmts), "duration_ms", time.Since(start).Milliseconds())
	return nil
}

// execPlan runs stmts in one transaction through the generated queriers. With epoch > 0 the
// transaction first reads cluster_state.epoch and refuses (ErrStaleEpoch) when it is higher.
// A plain read, no FOR UPDATE: a promotion racing this exact transaction can still land one batch,
// which the promoted leader's own writes then overwrite.
func execPlan(ctx context.Context, store *db.Store, stmts []stmt, epoch uint64) error {
	return store.ExecTx(ctx, func(tx *db.Store) error {
		if epoch > 0 {
			stored, err := tx.Cluster.GetEpoch(ctx)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("persist: flush: read epoch: %w", err)
			}
			if err := checkEpoch(uint64(stored), epoch); err != nil {
				return err
			}
		}
		for _, s := range stmts {
			if err := execStmt(ctx, tx, s); err != nil {
				return fmt.Errorf("persist: flush %s %q: %w", s.kind, s.sheet, err)
			}
		}
		return nil
	})
}

// oldestTerm is the lowest non-zero acceptance term in qs (0 when none carries one: no fencing).
func oldestTerm(qs []queued) uint64 {
	var low uint64
	for _, q := range qs {
		if q.epoch != 0 && (low == 0 || q.epoch < low) {
			low = q.epoch
		}
	}
	return low
}

// checkEpoch refuses a flush by a node whose epoch is below the stored one.
func checkEpoch(stored, mine uint64) error {
	if stored > mine {
		return fmt.Errorf("persist: flush at epoch %d, stored %d: %w", mine, stored, ErrStaleEpoch)
	}
	return nil
}

// execStmt maps one planned statement to its generated query.
func execStmt(ctx context.Context, tx *db.Store, s stmt) error {
	switch s.kind {
	case stmtUpsertSheet:
		return tx.Registry.UpsertSheet(ctx, registry.UpsertSheetParams{
			ProjectID: s.project, Name: s.sheet, PasswordHash: s.passwordHash, Autocreated: s.autocreated,
		})
	case stmtUpsertEntry:
		return tx.Entries.UpsertEntry(ctx, entries.UpsertEntryParams{
			ProjectID: s.project, Sheet: s.sheet, Key: s.match, Value: s.value,
			ExpiresAt: pgtype.Timestamptz{Time: s.expiresAt, Valid: !s.expiresAt.IsZero()},
		})
	case stmtDeleteEntry:
		return tx.Entries.DeleteEntry(ctx, entries.DeleteEntryParams{ProjectID: s.project, Sheet: s.sheet, Key: s.match})
	case stmtDeletePrefix:
		_, err := tx.Entries.DeleteEntriesByPrefix(ctx, entries.DeleteEntriesByPrefixParams{ProjectID: s.project, Sheet: s.sheet, Prefix: s.match})
		return err
	case stmtDeleteSuffix:
		_, err := tx.Entries.DeleteEntriesBySuffix(ctx, entries.DeleteEntriesBySuffixParams{ProjectID: s.project, Sheet: s.sheet, Suffix: s.match})
		return err
	case stmtDeleteEntries:
		return tx.Entries.DeleteEntries(ctx, entries.DeleteEntriesParams{ProjectID: s.project, Sheet: s.sheet, Keys: s.keys})
	}
	return fmt.Errorf("persist: unknown statement kind %d", s.kind)
}

// String names the statement for error messages.
func (k stmtKind) String() string {
	switch k {
	case stmtUpsertSheet:
		return "upsert_sheet"
	case stmtUpsertEntry:
		return "upsert_entry"
	case stmtDeleteEntry:
		return "delete_entry"
	case stmtDeletePrefix:
		return "delete_prefix"
	case stmtDeleteSuffix:
		return "delete_suffix"
	case stmtDeleteEntries:
		return "delete_entries"
	}
	return "unknown"
}
