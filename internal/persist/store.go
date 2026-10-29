package persist

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/CrimsonCoder42/red_lite_cache/db"
	"github.com/CrimsonCoder42/red_lite_cache/db/sqlc/cluster"
	"github.com/CrimsonCoder42/red_lite_cache/db/sqlc/entries"
	"github.com/CrimsonCoder42/red_lite_cache/internal/cache"
	"github.com/CrimsonCoder42/red_lite_cache/internal/logger"
)

// SheetRow is one persisted sheet handed to Hydrate's load.
type SheetRow struct {
	ProjectID    uuid.UUID
	Name         string
	PasswordHash []byte
}

// EntryRow is one live persisted entry of a sheet.
type EntryRow struct {
	Key   string
	Entry cache.Entry
}

// Hydrate reads every sheet and its live entries (expired rows are filtered by the query, never
// loaded) and calls load once per sheet, in name order. It returns the sheets and entries loaded.
// It does not log: a failure is the caller's to log ("Persist: hydration failed").
// after advo_cache/CACHE_PERSISTENCE_REPORT.md:787-815 (Loader.LoadAllEntries): one query per
// sheet instead of one over the whole table, the value kept as raw JSON (never decoded), the
// absolute expires_at kept instead of a recomputed remaining TTL.
func Hydrate(ctx context.Context, store *db.Store, load func(SheetRow, []EntryRow) error) (sheets, entryCount int, err error) {
	rows, err := store.Registry.ListSheets(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("persist: hydrate: list sheets: %w", err)
	}
	for _, s := range rows {
		live, err := store.Entries.ListLiveEntries(ctx, entries.ListLiveEntriesParams{ProjectID: s.ProjectID, Sheet: s.Name})
		if err != nil {
			return sheets, entryCount, fmt.Errorf("persist: hydrate %q: list entries: %w", s.Name, err)
		}
		out := make([]EntryRow, len(live))
		for i, r := range live {
			e := cache.Entry{Value: r.Value}
			if r.ExpiresAt.Valid {
				e.ExpiresAt = r.ExpiresAt.Time
			}
			out[i] = EntryRow{Key: r.Key, Entry: e}
		}
		if err := load(SheetRow{ProjectID: s.ProjectID, Name: s.Name, PasswordHash: s.PasswordHash}, out); err != nil {
			return sheets, entryCount, fmt.Errorf("persist: hydrate %q: load: %w", s.Name, err)
		}
		sheets++
		entryCount += len(out)
	}
	return sheets, entryCount, nil
}

// RunCleaner deletes expired rows (at most 10000 a pass, DeleteExpiredEntries) every interval,
// then runs each after func (the control-bus reconcile, D-021),
// until ctx ends, then returns nil. A failed pass logs at Error and the cleaner keeps running. Its
// own delete never touches memory (an after func may). interval <= 0 uses the 60s default.
func RunCleaner(ctx context.Context, store *db.Store, interval time.Duration, after ...func(context.Context)) error {
	if interval <= 0 {
		interval = defaultCleanupInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			n, err := store.Entries.DeleteExpiredEntries(ctx)
			switch {
			case ctx.Err() != nil:
				return nil
			case err != nil:
				logger.L.Error("Persist: cleanup failed", "err", err)
			default:
				logger.L.Debug("Persist: cleanup pass", "rows_deleted", n)
			}
			for _, fn := range after {
				fn(ctx)
			}
		}
	}
}

// LoadEpoch returns the stored fencing epoch (D-012), 0 when no row exists yet.
func LoadEpoch(ctx context.Context, store *db.Store) (uint64, error) {
	epoch, err := store.Cluster.GetEpoch(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("persist: load epoch: %w", err)
	}
	return uint64(epoch), nil
}

// RaiseEpoch stores epoch when it is higher than the stored one and returns the epoch now stored;
// the stored epoch never goes backwards.
func RaiseEpoch(ctx context.Context, store *db.Store, epoch uint64, instanceID string) (uint64, error) {
	if epoch > math.MaxInt64 {
		return 0, fmt.Errorf("persist: raise epoch %d: exceeds bigint", epoch)
	}
	stored, err := store.Cluster.RaiseEpoch(ctx, cluster.RaiseEpochParams{Epoch: int64(epoch), InstanceID: instanceID})
	if err != nil {
		return 0, fmt.Errorf("persist: raise epoch %d: %w", epoch, err)
	}
	return uint64(stored), nil
}
