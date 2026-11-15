package persist

import (
	"context"
	"time"

	"github.com/CrimsonCoder42/red_lite_cache/db"
	"github.com/CrimsonCoder42/red_lite_cache/db/sqlc/admin"
	"github.com/CrimsonCoder42/red_lite_cache/internal/adminauth"
	"github.com/CrimsonCoder42/red_lite_cache/internal/logger"
)

// AdminSessionsPurge returns a RunCleaner after-func that deletes admin portal sessions past their
// absolute expiry or idle for adminauth.SessionIdle (S11; admin_sessions_purge in migration 000003).
// A session the purge has not reached yet is already refused by admin_session_check, so the purge
// only bounds the table. A failed pass logs at Error; the next tick retries.
func AdminSessionsPurge(store *db.Store) func(context.Context) {
	q := admin.New(store.Pool())
	return func(ctx context.Context) {
		n, err := q.AdminSessionsPurge(ctx, int32(adminauth.SessionIdle/time.Second))
		switch {
		case ctx.Err() != nil:
		case err != nil:
			logger.L.Error("Persist: admin session purge failed", "err", err)
		case n > 0:
			logger.L.Debug("Persist: admin sessions purged", "rows_deleted", n)
		}
	}
}
