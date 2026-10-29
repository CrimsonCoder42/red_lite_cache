package app

// The replica node and the persistence layer wired into App: New calls wireReplicaPersist, Run
// calls runReplicaPersist, and Run's shutdown calls connsClosed once every client is gone.

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"

	"github.com/CrimsonCoder42/red_lite_cache/db"
	"github.com/CrimsonCoder42/red_lite_cache/internal/api"
	"github.com/CrimsonCoder42/red_lite_cache/internal/cache"
	"github.com/CrimsonCoder42/red_lite_cache/internal/logger"
	"github.com/CrimsonCoder42/red_lite_cache/internal/persist"
	"github.com/CrimsonCoder42/red_lite_cache/internal/replica"
	"github.com/CrimsonCoder42/red_lite_cache/internal/sheet"
	"github.com/CrimsonCoder42/red_lite_cache/protocol"
)

// wiredNode is App.node: the replica node (ws.Node through embedding) plus the persistence handles
// App exposes (Store, Writer).
type wiredNode struct {
	*replica.Node
	store  *db.Store       // nil when persistence is off
	writer *persist.Writer // nil when persistence is off

	// persistCtx runs the writer and the cleaner. Its cancel (connsClosed) runs only after the HTTP
	// server shut down and every /ws connection closed, so ops applied while clients disconnect
	// are still flushed (review HIGH: shutdown ordering). Set in New.
	persistCtx  context.Context
	stopPersist context.CancelFunc
}

// Node is the instance's replica node (role, epoch, replicas).
func (a *App) Node() *replica.Node { return a.wired().Node }

// Store is the persistence store, nil when REDLITE_PERSIST_ENABLED is false.
func (a *App) Store() *db.Store { return a.wired().store }

// Writer is the persist writer, nil when persistence is off.
func (a *App) Writer() *persist.Writer { return a.wired().writer }

func (a *App) wired() *wiredNode { return a.node.(*wiredNode) }

// connsClosed ends the writer and the cleaner: Run calls it after every client connection closed.
// Idempotent.
func (a *App) connsClosed() { a.wired().stopPersist() }

// wireReplicaPersist builds the node and, when persistence is on, migrates, sets cache_entries'
// durability (D-020), opens the pool, builds the advisory elector (D-022), hydrates a leader's
// registry (before the listener exists), and registers the observers. On an error everything it
// opened is closed.
func (a *App) wireReplicaPersist(ctx context.Context) (err error) {
	cfg := a.cfg
	w := &wiredNode{}
	w.persistCtx, w.stopPersist = context.WithCancel(context.WithoutCancel(ctx))
	var elector replica.Elector // nil interface in epoch mode
	if cfg.PersistEnabled {
		if err := db.Migrate(cfg.PersistDatabaseURL); err != nil {
			return fmt.Errorf("app: persist: %w", err)
		}
		pool, err := db.OpenPool(ctx, cfg.PersistDatabaseURL, int32(cfg.PersistPoolMin), int32(cfg.PersistPoolMax))
		if err != nil {
			return fmt.Errorf("app: persist: %w", err)
		}
		w.store = db.NewStore(pool)
		defer func() {
			if err != nil {
				pool.Close()
			}
		}()
		if err := persist.SetEntriesLogged(ctx, w.store, cfg.PersistLogged); err != nil {
			return fmt.Errorf("app: persist: %w", err)
		}
		if cfg.Election == "advisory" { // config refuses advisory without persistence
			elector = persist.NewElector(w.store)
		}
	}

	role := replica.RoleLeader
	if cfg.IsReplica() {
		role = replica.RoleReplica
	}
	node, err := replica.New(ctx, a.reg, replica.Options{
		Role: role, LeaderAddr: cfg.LeaderAddr, PeerAddr: cfg.PeerAddr, InstanceID: a.instanceID,
		HeartbeatInterval: cfg.HeartbeatInterval, HeartbeatTimeout: cfg.HeartbeatTimeout,
		SendQueue: cfg.ReplicaSendQueue, MaxFrameBytes: cfg.ReplicaMaxFrameBytes,
		PingInterval: cfg.PingInterval, WriteTimeout: cfg.WriteTimeout,
		Store:       w.store,
		KickClients: func(reason string) { a.ws.KickAll(reason) }, // a.ws is set later in New
		OnPromote:   a.onPromote,
		Elector:     elector,
	})
	if err != nil {
		return fmt.Errorf("app: %w", err)
	}
	w.Node = node
	a.node = w

	if w.store != nil {
		if node.IsLeader() {
			if err := a.hydrate(ctx); err != nil {
				return err
			}
		}
		w.writer = persist.NewWriter(w.store, persist.Options{
			BatchSize: cfg.PersistBatchSize, FlushInterval: cfg.PersistFlushInterval,
			QueueSize: cfg.PersistQueueSize, CleanupInterval: cfg.PersistCleanupInterval,
			Epoch: node.Epoch, // a fenced leader's flush is refused once a newer epoch is stored
		}, node.IsLeader)
		a.reg.AddObserver(w.writer)
	}
	a.reg.AddObserver(node)
	a.mux.Handle(protocol.PathReplica, node)
	return nil
}

// hydrate loads every persisted sheet (hash, project) and its live entries into the registry.
// Seq starts at 0 for a hydrated sheet. A failure is logged and fails start-up.
func (a *App) hydrate(ctx context.Context) error {
	sheets, entries, err := persist.Hydrate(ctx, a.wired().store, func(row persist.SheetRow, rows []persist.EntryRow) error {
		s := a.reg.Restore(row.ProjectID, row.Name, row.PasswordHash)
		m := make(map[string]cache.Entry, len(rows))
		for _, r := range rows {
			m[r.Key] = r.Entry
		}
		s.Replace(sheet.Snapshot{Name: row.Name, ProjectID: row.ProjectID, PasswordHash: row.PasswordHash, Entries: m})
		return nil
	})
	if err != nil {
		logger.L.Error("Persist: hydration failed", "sheets", sheets, "entries", entries, "err", err)
		return fmt.Errorf("app: %w", err)
	}
	logger.L.Info("Persist: hydrated", "sheets", sheets, "entries", entries)
	return nil
}

// onPromote runs when this replica becomes leader. With persistence: a replica that never got a
// snapshot hydrates from the database (a lone replica serves what was persisted), and every sheet
// it holds is ensured in the database so its entries never fail the sheet foreign key.
func (a *App) onPromote(ctx context.Context, synced bool) {
	w := a.wired()
	if w.store == nil {
		return
	}
	if !synced {
		_ = a.hydrate(ctx) // logged inside; the node still promotes and serves what it has
	}
	for _, s := range a.reg.Sheets() {
		w.writer.EnsureSheet(sheet.Op{Sheet: s.Name(), ProjectID: s.ProjectID(), PasswordHash: s.PasswordHash()})
	}
}

// applyControl applies one control-bus payload to this instance's memory (D-021).
func (a *App) applyControl(c persist.Control) {
	switch c.Kind {
	case persist.KindProjectRevoked:
		id, err := uuid.Parse(c.Project)
		if err != nil {
			logger.L.Warn("Persist: control payload ignored", "kind", c.Kind, "err", err)
			return
		}
		dropped := api.DropProject(a.reg, id, nil)
		if w := a.wired().writer; w != nil {
			w.RevokeProject(id, dropped) // this instance's own queued ops of the project
		}
		logger.L.Info("Persist: project revoked applied", "project", id.String(), "sheets_dropped", len(dropped))
	default:
		logger.L.Debug("Persist: control kind ignored", "kind", c.Kind)
	}
}

// runReplicaPersist adds one goroutine to g: an inner errgroup running the node (on gctx) and,
// with persistence, the control listener (gctx), the writer and the TTL cleaner + reconcile (on
// persistCtx, ended by connsClosed, or ShutdownTimeout after gctx as a backstop); the pool is
// closed after all of them returned.
func (a *App) runReplicaPersist(g *errgroup.Group, gctx context.Context) {
	w := a.wired()
	g.Go(func() error {
		defer w.stopPersist()
		pg, pctx := errgroup.WithContext(gctx)
		pg.Go(func() error { return w.Run(pctx) })
		if w.store != nil {
			pg.Go(func() error { return w.writer.Run(w.persistCtx) })
			rec := persist.NewReconciler(w.store, func(name string) {
				a.reg.Drop(name, "sheet deleted")
				w.writer.RevokeSheets([]string{name})
			})
			pg.Go(func() error {
				return persist.RunCleaner(w.persistCtx, w.store, a.cfg.PersistCleanupInterval, rec.Pass)
			})
			if a.cfg.ControlListen {
				pg.Go(func() error { return persist.RunControlListener(pctx, w.store, a.applyControl) })
			}
			pg.Go(func() error { // backstop: never hold the writer past shutdown if connsClosed is missed
				<-pctx.Done()
				select {
				case <-w.persistCtx.Done():
				case <-time.After(a.cfg.ShutdownTimeout):
					logger.L.Warn("Persist: writer stopped by backstop", "after", a.cfg.ShutdownTimeout.String())
					w.stopPersist()
				}
				return nil
			})
		}
		err := pg.Wait()
		if w.store != nil {
			w.store.Pool().Close()
		}
		return err
	})
}
