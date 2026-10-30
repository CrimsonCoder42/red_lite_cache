package api

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/CrimsonCoder42/red_lite_cache/internal/logger"
)

// Key-use touches (D-028): api_keys.last_used_at at most once a minute per key, never per frame and
// never on the open path.
const (
	touchEvery   = time.Minute
	touchQueue   = 256
	touchTimeout = 2 * time.Second
)

// Toucher throttles key-use writes. Touch (tenant.Directory.OnAdmit) records the key in memory and,
// at most once a minute per key, hands it to Run's one goroutine without blocking; a full queue
// drops the touch (last_used_at is informational). Run writes it through Store.TouchKey.
//
// after code-heim go_18_worker_pool (doc/reference/codeheim_patterns.md § Tier 2): one worker on a
// bounded channel, ctx-cancelled; non-blocking enqueue and a once-a-minute gate added, and the
// gate map is pruned every minute so it never outgrows the keys used in the last minute.
type Toucher struct {
	store Store
	ch    chan uuid.UUID
	mu    sync.Mutex
	last  map[uuid.UUID]time.Time
	now   func() time.Time
}

// NewToucher builds a toucher writing through store.
func NewToucher(store Store) *Toucher {
	return &Toucher{store: store, ch: make(chan uuid.UUID, touchQueue), last: map[uuid.UUID]time.Time{}, now: time.Now}
}

// Touch records one admission with keyID. Never blocks.
func (t *Toucher) Touch(keyID uuid.UUID) {
	if keyID == uuid.Nil {
		return
	}
	now := t.now()
	t.mu.Lock()
	if at, ok := t.last[keyID]; ok && now.Sub(at) < touchEvery {
		t.mu.Unlock()
		return
	}
	t.last[keyID] = now
	t.mu.Unlock()
	select {
	case t.ch <- keyID:
	default:
		logger.L.Debug("API: key touch dropped", "key_id", keyID.String(), "cap", cap(t.ch))
	}
}

// Run writes queued touches until ctx ends, then returns nil.
func (t *Toucher) Run(ctx context.Context) error {
	prune := time.NewTicker(touchEvery)
	defer prune.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-prune.C:
			cut := t.now().Add(-touchEvery)
			t.mu.Lock()
			for id, at := range t.last {
				if at.Before(cut) {
					delete(t.last, id)
				}
			}
			t.mu.Unlock()
		case id := <-t.ch:
			tctx, cancel := context.WithTimeout(ctx, touchTimeout)
			err := t.store.TouchKey(tctx, id)
			cancel()
			if err != nil && ctx.Err() == nil {
				logger.L.Warn("API: key touch failed", "key_id", id.String(), "err", err)
			}
		}
	}
}
