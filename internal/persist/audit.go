package persist

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/CrimsonCoder42/red_lite_cache/db"
	"github.com/CrimsonCoder42/red_lite_cache/db/sqlc/audit"
	"github.com/CrimsonCoder42/red_lite_cache/internal/logger"
	"github.com/CrimsonCoder42/red_lite_cache/internal/tenant"
)

// Audit queue defaults (D-026, 000002).
const (
	defaultAuditQueue  = 1024
	auditWriteTimeout  = 2 * time.Second
	auditDrainTimeout  = 2 * time.Second
	auditWarnEvery     = time.Second // minimum gap between two drop / failure lines
	auditPartitionsAhd = 1           // this month plus this many following months
)

type auditRow struct {
	at time.Time
	e  tenant.Event
}

// Auditor is the persistent tenant.Auditor: Audit enqueues on a bounded channel without blocking
// (a full queue drops the row, Warn "Persist: audit dropped" at most once a second with the count)
// and one Run goroutine inserts each row into audit_events with node = this instance id.
//
// after code-heim go_18_worker_pool (doc/reference/codeheim_patterns.md § Tier 2), the same shape as
// the persist Writer: one worker, ctx-cancelled, non-blocking enqueue; rows are written one by one
// (a few per minute, never on the hot path) and what is queued at shutdown is drained with a deadline.
type Auditor struct {
	store *db.Store
	node  string
	ch    chan auditRow

	dropped     atomic.Uint64 // since the last drop Warn
	lastDropLog atomic.Int64
	failed      atomic.Uint64 // since the last failure Error
	lastFailLog atomic.Int64
}

// NewAuditor builds an auditor writing as node. queue <= 0 uses 1024.
func NewAuditor(store *db.Store, node string, queue int) *Auditor {
	if queue <= 0 {
		queue = defaultAuditQueue
	}
	return &Auditor{store: store, node: node, ch: make(chan auditRow, queue)}
}

// Audit is tenant.Auditor: enqueue e stamped now, or drop it when the queue is full. Never blocks.
func (a *Auditor) Audit(e tenant.Event) {
	select {
	case a.ch <- auditRow{at: time.Now(), e: e}:
	default:
		a.dropped.Add(1)
		if limited(&a.lastDropLog) {
			logger.L.Warn("Persist: audit dropped", "action", e.Action, "cap", cap(a.ch), "dropped", a.dropped.Swap(0))
		}
	}
}

// limited reports whether a rate-limited line may be logged now (at most one per auditWarnEvery).
func limited(last *atomic.Int64) bool {
	now := time.Now().UnixNano()
	prev := last.Load()
	return now-prev >= int64(auditWarnEvery) && last.CompareAndSwap(prev, now)
}

// Run inserts queued rows until ctx ends, then drains what is queued (bounded by 2s) and returns nil.
func (a *Auditor) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), auditDrainTimeout)
			defer cancel()
			for {
				select {
				case row := <-a.ch:
					a.write(dctx, row)
				default:
					return nil
				}
			}
		case row := <-a.ch:
			a.write(ctx, row)
		}
	}
}

func (a *Auditor) write(ctx context.Context, row auditRow) {
	detail := []byte("{}")
	if len(row.e.Detail) > 0 {
		b, err := json.Marshal(row.e.Detail)
		if err == nil {
			detail = b
		}
	}
	wctx, cancel := context.WithTimeout(ctx, auditWriteTimeout)
	defer cancel()
	err := a.store.Audit.InsertAuditEvent(wctx, audit.InsertAuditEventParams{
		At: row.at, Node: a.node, ProjectID: pgtype.UUID{Bytes: row.e.Project, Valid: row.e.Project != uuid.Nil},
		Actor: row.e.Actor, Action: row.e.Action, Subject: row.e.Subject, Detail: detail,
	})
	if err == nil {
		return
	}
	a.failed.Add(1)
	if limited(&a.lastFailLog) {
		logger.L.Error("Persist: audit write failed", "action", row.e.Action, "failed", a.failed.Swap(0), "err", err)
	}
}

// EnsureAuditPartitions creates audit_events' monthly partitions for the month of now and the next
// one (UTC), each `IF NOT EXISTS`; rows outside them land in audit_events_default. Run at boot and
// on every cleaner tick. The DDL is built from integers only.
func EnsureAuditPartitions(ctx context.Context, store *db.Store, now time.Time) error {
	start := time.Date(now.UTC().Year(), now.UTC().Month(), 1, 0, 0, 0, 0, time.UTC)
	for range auditPartitionsAhd + 1 {
		end := start.AddDate(0, 1, 0)
		ddl := fmt.Sprintf(
			"CREATE TABLE IF NOT EXISTS audit_events_%04d_%02d PARTITION OF audit_events FOR VALUES FROM ('%s') TO ('%s')",
			start.Year(), int(start.Month()), start.Format("2006-01-02 15:04:05+00"), end.Format("2006-01-02 15:04:05+00"))
		if _, err := store.Pool().Exec(ctx, ddl); err != nil {
			return fmt.Errorf("persist: audit partition %04d-%02d: %w", start.Year(), int(start.Month()), err)
		}
		start = end
	}
	return nil
}
