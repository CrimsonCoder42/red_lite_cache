package app

// The replica node and the persistence layer wired into App: New calls wireReplicaPersist, Run
// calls runReplicaPersist, and Run's shutdown calls connsClosed once every client is gone.

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"

	"github.com/CrimsonCoder42/red_lite_cache/db"
	"github.com/CrimsonCoder42/red_lite_cache/internal/api"
	"github.com/CrimsonCoder42/red_lite_cache/internal/cache"
	"github.com/CrimsonCoder42/red_lite_cache/internal/logger"
	"github.com/CrimsonCoder42/red_lite_cache/internal/persist"
	"github.com/CrimsonCoder42/red_lite_cache/internal/replica"
	"github.com/CrimsonCoder42/red_lite_cache/internal/session"
	"github.com/CrimsonCoder42/red_lite_cache/internal/sheet"
	"github.com/CrimsonCoder42/red_lite_cache/internal/tenant"
	"github.com/CrimsonCoder42/red_lite_cache/protocol"
)

// wiredNode is App.node: the replica node (ws.Node through embedding) plus the persistence handles
// App exposes (Store, Writer).
type wiredNode struct {
	*replica.Node
	store  *db.Store       // nil when persistence is off
	writer *persist.Writer // nil when persistence is off

	auditor tenant.Auditor   // persist.Auditor with persistence, else tenant.NopAuditor
	paudit  *persist.Auditor // nil when persistence is off (Run)
	toucher *api.Toucher     // last_used_at throttle (D-028); set by wireTenancy

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

// Auditor is the audit sink (D-026): audit_events through a bounded queue with persistence,
// NopAuditor without. Built before /ws, so the ws handler and the admin API share it.
func (a *App) Auditor() tenant.Auditor { return a.wired().auditor }

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
		if err := persist.EnsureAuditPartitions(ctx, w.store, time.Now()); err != nil {
			logger.L.Warn("Persist: audit partition failed", "err", err) // rows land in audit_events_default
		}
		w.paudit = persist.NewAuditor(w.store, a.instanceID, 0)
		w.auditor = w.paudit
	} else {
		w.auditor = tenant.NopAuditor{}
	}

	role := replica.RoleLeader
	if cfg.IsReplica() {
		role = replica.RoleReplica
	}
	node, err := replica.New(ctx, a.reg, replica.Options{
		Role: role, LeaderAddr: cfg.LeaderAddr, PeerAddr: cfg.PeerAddr, ClusterToken: cfg.ClusterToken, InstanceID: a.instanceID,
		HeartbeatInterval: cfg.HeartbeatInterval, HeartbeatTimeout: cfg.HeartbeatTimeout,
		SendQueue: cfg.ReplicaSendQueue, MaxFrameBytes: cfg.ReplicaMaxFrameBytes,
		PingInterval: cfg.PingInterval, WriteTimeout: cfg.WriteTimeout,
		Store:       w.store,
		KickClients: func(reason string) { a.ws.KickAll(reason) }, // a.ws is set later in New
		OnPromote:   a.onPromote,
		OnControl:   func(c protocol.ReplicaControl) { a.applyTenancy(c, "stream") },
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
		if a.tenants != nil {
			a.tenants.MarkRevoked(dropped...) // autocreate never recreates them (D-025)
		}
		logger.L.Info("Persist: project revoked applied", "project", id.String(), "sheets_dropped", len(dropped))
	case persist.KindProjectSuspended, persist.KindProjectResumed, persist.KindKeyRevoked, persist.KindSessionKill:
		a.applyTenancy(protocol.ReplicaControl{Type: c.Kind, Project: c.Project, KeyID: c.KeyID, Session: c.Session}, "notify")
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
		if w.toucher != nil {
			pg.Go(func() error { return w.toucher.Run(pctx) })
		}
		if w.store != nil {
			pg.Go(func() error { return w.writer.Run(w.persistCtx) })
			pg.Go(func() error { return w.paudit.Run(w.persistCtx) }) // after every conn closed: session_close rows land
			rec := persist.NewReconciler(w.store, func(name string) {
				a.reg.Drop(name, "sheet deleted")
				w.writer.RevokeSheets([]string{name})
			})
			pg.Go(func() error {
				return persist.RunCleaner(w.persistCtx, w.store, a.cfg.PersistCleanupInterval,
					rec.Pass, a.reconcileSuspended, a.ensureAuditPartitions)
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

// ── tenancy wiring (D-024…D-031) ──

// wireTenancy finishes the tenancy state New built (a.projects, a.tenants, a.sessions) before
// any listener exists: the suspended set is loaded from the store (Postgres with persistence),
// admissions feed the last_used_at toucher, and the admin API gets its dependencies.
func (a *App) wireTenancy(ctx context.Context) (api.Options, error) {
	w := a.wired()
	suspended, err := a.projects.ListSuspended(ctx)
	if err != nil {
		return api.Options{}, fmt.Errorf("app: load suspended projects: %w", err)
	}
	a.tenants.ReplaceSuspended(suspended)
	if len(suspended) > 0 {
		logger.L.Info("API: suspended projects loaded", "projects", len(suspended))
	}
	w.toucher = api.NewToucher(a.projects)
	a.tenants.OnAdmit(w.toucher.Touch)
	return api.Options{
		Tenants: a.tenants, Sessions: a.sessions, Auditor: w.auditor,
		Propagate: a.propagate, PeerURL: peerURL(a.cfg.PeerAddr, a.cfg.LeaderAddr, a.cfg.IsReplica()),
	}, nil
}

// propagate carries a tenancy change to the other instances: to every connected replica over the
// stream (a no-op without one) and, with persistence, by NOTIFY redlite_control too (a replica
// that misses the stream frame, or an instance off the stream). Applying twice is harmless: every
// apply is idempotent.
func (a *App) propagate(c protocol.ReplicaControl) {
	w := a.wired()
	w.SendControl(c)
	if w.store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := persist.Notify(ctx, w.store, persist.Control{Kind: c.Type, Project: c.Project, KeyID: c.KeyID, Session: c.Session}); err != nil {
		logger.L.Error("API: control notify failed", "type", c.Type, "project", c.Project, "err", err)
	}
}

// applyTenancy applies one tenancy control (from the leader's stream or the NOTIFY bus) to this
// instance: suspend closes the project's sessions here (4001) and refuses opens, resume lifts it,
// key_revoked closes that key's sessions (4002), session_kill closes that session if held (4003).
func (a *App) applyTenancy(c protocol.ReplicaControl, source string) {
	if a.tenants == nil || a.sessions == nil { // before New finished: nothing is admitted yet
		return
	}
	n := 0
	switch c.Type {
	case protocol.TypeProjectSuspended, protocol.TypeProjectResumed:
		id, err := uuid.Parse(c.Project)
		if err != nil {
			logger.L.Warn("Replica: control ignored", "type", c.Type, "source", source, "err", err)
			return
		}
		suspended := c.Type == protocol.TypeProjectSuspended
		a.tenants.SetSuspended(id, suspended)
		if suspended {
			n = a.sessions.KillWhere(session.Filter{Project: id}, protocol.CloseSuspended, protocol.CloseReasonSuspended)
		}
	case protocol.TypeKeyRevoked:
		if c.KeyID == "" { // a project revoke: every key of the project, every session closes 4002
			pid, err := uuid.Parse(c.Project)
			if err != nil || pid == uuid.Nil {
				logger.L.Warn("Replica: control ignored", "type", c.Type, "source", source, "err", err)
				return
			}
			n = a.sessions.KillWhere(session.Filter{Project: pid}, protocol.CloseKeyRevoked, protocol.CloseReasonKeyRevoked)
			break
		}
		id, err := uuid.Parse(c.KeyID)
		if err != nil || id == uuid.Nil { // a Nil filter would match every session
			logger.L.Warn("Replica: control ignored", "type", c.Type, "source", source, "err", err)
			return
		}
		a.tenants.MarkKeyRevoked(id)
		n = a.sessions.KillWhere(session.Filter{KeyID: id}, protocol.CloseKeyRevoked, protocol.CloseReasonKeyRevoked)
	case protocol.TypeSessionKill:
		id, err := uuid.Parse(c.Session)
		if err != nil {
			logger.L.Warn("Replica: control ignored", "type", c.Type, "source", source, "err", err)
			return
		}
		if s, ok := a.sessions.Get(id); ok && s.Kill(protocol.CloseKicked, protocol.CloseReasonKicked) {
			n = 1
		}
	default:
		logger.L.Debug("Replica: control type ignored", "type", c.Type, "source", source)
		return
	}
	logger.L.Info("Replica: control applied", "type", c.Type, "project", c.Project, "key_id", c.KeyID,
		"session", c.Session, "sessions_closed", n, "source", source)
}

// reconcileSuspended is the cleaner-tick fallback for a missed suspend/resume NOTIFY: the
// suspended set is replaced by Postgres's, and sessions of a project newly found suspended close.
func (a *App) reconcileSuspended(ctx context.Context) {
	ids, err := a.projects.ListSuspended(ctx)
	if err != nil {
		if ctx.Err() == nil {
			logger.L.Error("Persist: suspend reconcile failed", "err", err)
		}
		return
	}
	var newly []uuid.UUID
	for _, id := range ids {
		if !a.tenants.Suspended(id) {
			newly = append(newly, id)
		}
	}
	a.tenants.ReplaceSuspended(ids)
	closed := 0
	for _, id := range newly {
		closed += a.sessions.KillWhere(session.Filter{Project: id}, protocol.CloseSuspended, protocol.CloseReasonSuspended)
	}
	if len(newly) > 0 {
		logger.L.Info("Persist: suspend reconciled", "projects", len(newly), "sessions_closed", closed)
	}
}

// ensureAuditPartitions keeps this and next month's audit_events partitions present (cleaner tick).
func (a *App) ensureAuditPartitions(ctx context.Context) {
	if err := persist.EnsureAuditPartitions(ctx, a.wired().store, time.Now()); err != nil && ctx.Err() == nil {
		logger.L.Warn("Persist: audit partition failed", "err", err)
	}
}

// peerURL is the other node's http(s)://host:port for the admin API's peer calls: REDLITE_PEER_ADDRESS,
// else a replica's leader address; "" when neither is set or it does not parse.
func peerURL(peerAddr, leaderAddr string, isReplica bool) string {
	addr := peerAddr
	if addr == "" && isReplica {
		addr = leaderAddr
	}
	if addr == "" {
		return ""
	}
	u, err := url.Parse(addr)
	if err != nil || u.Host == "" {
		logger.L.Warn("API: peer address ignored", "err", err)
		return ""
	}
	switch u.Scheme {
	case "ws":
		u.Scheme = "http"
	case "wss":
		u.Scheme = "https"
	}
	u.Path, u.RawQuery, u.Fragment = "", "", ""
	return u.String()
}
