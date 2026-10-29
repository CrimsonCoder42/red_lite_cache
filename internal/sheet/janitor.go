package sheet

import (
	"context"
	"time"

	"github.com/CrimsonCoder42/red_lite_cache/internal/logger"
)

// RunJanitor is the ONE expiry goroutine of the process (D-003): every interval it runs ExpireAll
// over the registry, skipping the pass while active() is false (a replica applies the leader's
// replicate_expire instead). It returns nil when ctx is done. The caller owns the goroutine
// (app's errgroup); RunJanitor starts none.
//
// after code-heim go_18_worker_pool (doc/reference/codeheim_patterns.md § Tier 2): one ctx-cancelled
// ticker loop; replaces advo_cache/cache/cache.go:44-51, a sleeping goroutine per Set (K5).
func RunJanitor(ctx context.Context, r *Registry, interval time.Duration, active func() bool) error {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if active != nil && !active() {
				continue
			}
			if n := r.ExpireAll(r.now()); n > 0 {
				logger.L.Debug("Sheet: janitor pass", "expired", n, "sheets", r.Len())
			}
		}
	}
}
