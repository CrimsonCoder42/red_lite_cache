package persist

import (
	"context"
	"fmt"

	"github.com/CrimsonCoder42/red_lite_cache/db"
	"github.com/CrimsonCoder42/red_lite_cache/internal/logger"
)

// SetEntriesLogged makes cache_entries LOGGED (logged=true) or UNLOGGED (false), so
// REDLITE_PERSIST_LOGGED is authoritative at every boot (D-020). It reads pg_class first and runs
// the ALTER (an ACCESS EXCLUSIVE lock and a table rewrite) only when the table is not already in the
// wanted state. Run after Migrate, before hydration. Raw SQL: DDL, not a query package.
func SetEntriesLogged(ctx context.Context, store *db.Store, logged bool) error {
	var persistence string
	if err := store.Pool().QueryRow(ctx,
		"SELECT relpersistence::text FROM pg_class WHERE oid = 'cache_entries'::regclass").Scan(&persistence); err != nil {
		return fmt.Errorf("persist: cache_entries persistence: %w", err)
	}
	isLogged := persistence == "p"
	if isLogged != logged {
		stmt := "ALTER TABLE cache_entries SET UNLOGGED"
		if logged {
			stmt = "ALTER TABLE cache_entries SET LOGGED"
		}
		if _, err := store.Pool().Exec(ctx, stmt); err != nil {
			return fmt.Errorf("persist: %s: %w", stmt, err)
		}
	}
	logger.L.Info("Persist: cache_entries durability", "logged", logged, "changed", isLogged != logged)
	return nil
}
