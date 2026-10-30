// Package app wires one red_lite_cache instance: the sheet registry, the lock manager, the node
// (leader or replica), the /ws handler and the HTTP mux, and runs them under one errgroup.
// cmd/redlite and the integration harness both build an instance through New + Run.
package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"runtime"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/CrimsonCoder42/red_lite_cache/internal/api"
	"github.com/CrimsonCoder42/red_lite_cache/internal/cache"
	"github.com/CrimsonCoder42/red_lite_cache/internal/config"
	"github.com/CrimsonCoder42/red_lite_cache/internal/lock"
	"github.com/CrimsonCoder42/red_lite_cache/internal/logger"
	"github.com/CrimsonCoder42/red_lite_cache/internal/session"
	"github.com/CrimsonCoder42/red_lite_cache/internal/sheet"
	"github.com/CrimsonCoder42/red_lite_cache/internal/tenant"
	"github.com/CrimsonCoder42/red_lite_cache/internal/ws"
	"github.com/CrimsonCoder42/red_lite_cache/protocol"
)

// App is one instance. Build it with New, mount extra routes with Handle, then Run it once.
type App struct {
	cfg        config.Config
	instanceID string
	reg        *sheet.Registry
	locks      *lock.Manager
	node       ws.Node
	ws         *ws.Handler
	mux        *http.ServeMux

	projects api.Store         // projects and keys: Postgres with persistence, else memory
	tenants  *tenant.Directory // admission + suspension + revoked names (D-024, D-025, D-027)
	sessions *session.Registry // this node's live sessions (D-026)
}

// Tenants is the admission directory every open consults.
func (a *App) Tenants() *tenant.Directory { return a.tenants }

// Sessions is this node's live-session registry.
func (a *App) Sessions() *session.Registry { return a.sessions }

// Projects is the project/key store (MemStore or DBStore).
func (a *App) Projects() api.Store { return a.projects }

// New builds an instance from cfg: registry, locks, the replica node, persistence (migrate, pool,
// hydration of a leader before any listener exists), /ws, /replica, /health and /admin/*. ctx
// bounds that start-up work.
//
// after advo_cache/main.go:28-98: the inline wiring and a bare goroutine per background loop became
// one constructor and one errgroup (Run).
func New(ctx context.Context, cfg config.Config) (*App, error) {
	id := cfg.InstanceID
	if id == "" {
		b := make([]byte, 8)
		if _, err := rand.Read(b); err != nil {
			return nil, fmt.Errorf("app: instance id: %w", err)
		}
		id = hex.EncodeToString(b) // after advo_cache/main.go:22-26 (generateInstanceID)
	}
	a := &App{
		cfg:        cfg,
		instanceID: id,
		reg: sheet.NewRegistry(sheet.Options{
			Policy:         cache.Policy{DefaultTTL: cfg.CacheTTL, JitterPercent: cfg.CacheJitterPercent},
			MaxKeys:        cfg.CacheMaxKeysPerSheet,
			Autocreate:     cfg.SheetAutocreate,
			BcryptCost:     cfg.BcryptCost,
			SweepLogWindow: cfg.SweepLogWindow,
			SweepLogMax:    cfg.SweepLogMax,
			Now:            cfg.Clock, // nil = time.Now (D-039 test seam)
		}),
		locks: lock.NewManager(cfg.Clock),
		mux:   http.NewServeMux(),
	}

	// ── replica + persist wiring ──
	// a.node becomes the replica node; with persistence: migrate, pool, hydrate a leader's a.reg
	// (before any listener exists), the persist writer as an observer; the replica hub as an
	// observer; protocol.PathReplica mounted. See wire_replica.go.
	if err := a.wireReplicaPersist(ctx); err != nil {
		return nil, err
	}
	// ── end replica + persist wiring ──

	// Tenancy (D-024…D-031): the project/key store, the admission directory and the session
	// registry, built before /ws so the router can admit against them.
	a.projects = api.NewMemStore()
	if store := a.Store(); store != nil {
		a.projects = api.NewDBStore(store)
	}
	a.tenants = tenant.New(a.projects, cfg.KeyAdmission())
	a.tenants.SetMemoryMax(cfg.TenantMemoryMax) // D-041
	a.sessions = session.NewRegistry(a.instanceID)
	if cfg.ClusterToken == "" {
		// production refuses to start without one (internal/config); elsewhere say it once.
		logger.L.Warn("Replica: cluster token empty, /replica and peer /health unauthenticated", "environment", cfg.Environment)
	}

	a.ws = ws.NewHandler(a.reg, a.locks, a.node, a.tenants, a.sessions, a.Auditor(), ws.Options{
		SendQueue:     cfg.ConnSendQueue,
		MaxFrameBytes: cfg.MaxFrameBytes,
		PingInterval:  cfg.PingInterval,
		WriteTimeout:  cfg.WriteTimeout,
		LockTTL:       cfg.LockTTL,
	})
	a.mux.HandleFunc("GET "+protocol.PathHealth, a.health)
	a.mux.Handle(protocol.PathWS, a.ws)

	// S6 management API over the same project store.
	admOpts, err := a.wireTenancy(ctx) // suspended set, last_used toucher, peer (wire_replica.go)
	if err != nil {
		return nil, err
	}
	adm := api.New(a.reg, a.projects, cfg.AdminKey, a.node.IsLeader, admOpts)
	if wr := a.Writer(); wr != nil {
		adm.OnRevoke(wr.RevokeProject) // queued ops of revoked sheets never reach Postgres
	}
	adm.Register(a.mux)
	return a, nil
}

// Handle mounts h on pattern (Go 1.22 mux patterns, e.g. "/admin/"). Call it before Run.
func (a *App) Handle(pattern string, h http.Handler) { a.mux.Handle(pattern, h) }

// Registry is the instance's sheet registry (admin API, tests).
func (a *App) Registry() *sheet.Registry { return a.reg }

// Locks is the instance's lock manager (tests, /health stats).
func (a *App) Locks() *lock.Manager { return a.locks }

// Config is the config the instance was built from.
func (a *App) Config() config.Config { return a.cfg }

// InstanceID is REDLITE_INSTANCE_ID or the id generated at New.
func (a *App) InstanceID() string { return a.instanceID }

// Conns is the number of live client connections on /ws.
func (a *App) Conns() int { return a.ws.Len() }

func (a *App) role() string {
	if a.node.IsLeader() {
		return "leader"
	}
	return "replica"
}

// health answers GET /health: a peer reads role and epoch to fence a stale leader (D-012).
// health always answers 200 {"status":"ok"} (Docker HEALTHCHECK, load balancers). Role, epoch and
// instance id — what a peer reads to fence a stale leader — are returned only to a caller with the
// cluster token (D-031); with no token configured (development) everyone gets them.
func (a *App) health(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	h := protocol.Health{Status: "ok"}
	if tenant.CheckBearer(r, a.cfg.ClusterToken) {
		h.Role, h.Epoch, h.InstanceID = a.role(), a.node.Epoch(), a.instanceID
	}
	if a.cfg.Environment == "development" && h.Role != "" {
		h.Stats = a.Stats() // D-040: development only, and like role/epoch only to a cluster-token holder (D-031)
	}
	json.NewEncoder(w).Encode(h)
}

// QueuedFrames is the number of frames waiting in every client connection's send queue.
func (a *App) QueuedFrames() int { return a.ws.QueuedFrames() }

// Stats is this node's sizes (D-040): counts only, never keys or values. /health carries it in
// development; the leak suite reads it.
func (a *App) Stats() *protocol.HealthStats {
	rs := a.reg.Stats()
	ts := a.tenants.Stats()
	st := &protocol.HealthStats{
		Sheets: rs.Sheets, Collaborators: rs.Collaborators, Entries: rs.Store.Entries,
		RootBuckets: rs.Store.RootBuckets, TailBuckets: rs.Store.TailBuckets, HeadBuckets: rs.Store.HeadBuckets,
		SweepLogEntries: rs.SweepLogEntries, Conns: a.ws.Len(), Sessions: a.sessions.Len(),
		LocksHeld: a.locks.Stats().Held, Suspended: ts.Suspended, RevokedNames: ts.RevokedNames, DeadKeys: ts.DeadKeys,
		Goroutines: runtime.NumGoroutine(),
	}
	if wr := a.Writer(); wr != nil {
		ps := wr.Stats()
		st.PersistQueued, st.PersistParked, st.PersistHeld, st.PersistDropped = ps.Queued, ps.Parked, ps.Held, ps.Dropped
	}
	return st
}

// Run serves on ln until ctx is done, then shuts down: stop accepting, kick every client, wait for
// connections and background loops (bounded by REDLITE_SHUTDOWN_TIMEOUT). It returns nil on a clean
// stop, or the first loop's error (which also stops everything else).
//
// after code-heim go_61_err_group (doc/reference/codeheim_patterns.md § Tier 2): one
// errgroup.WithContext owns every goroutine; the first error cancels the rest. Replaces
// advo_cache/main.go's bare goroutines and ListenAndServe with no shutdown.
func (a *App) Run(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{Handler: a.mux, ReadHeaderTimeout: 10 * time.Second}
	g, gctx := errgroup.WithContext(ctx)

	g.Go(func() error {
		logger.L.Info("WS: listening", "addr", ln.Addr().String(), "role", a.role())
		if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
			logger.L.Error("WS: listener closed", "addr", ln.Addr().String(), "err", err)
			return fmt.Errorf("app: serve %s: %w", ln.Addr(), err)
		}
		return nil
	})
	g.Go(func() error { return sheet.RunJanitor(gctx, a.reg, a.cfg.CacheJanitorInterval, a.node.IsLeader) })
	g.Go(func() error { return a.locks.RunCleaner(gctx, a.cfg.LockCleanupInterval) })

	// ── replica + persist wiring ──
	// The replica node, the persist writer and the persist TTL cleaner, on gctx; the pool closes
	// after all three returned (wire_replica.go).
	a.runReplicaPersist(g, gctx)
	// ── end replica + persist wiring ──

	g.Go(func() error {
		<-gctx.Done()
		sctx, cancel := context.WithTimeout(context.WithoutCancel(gctx), a.cfg.ShutdownTimeout)
		defer cancel()
		err := srv.Shutdown(sctx) // closes the listener; hijacked sockets are not waited for
		a.ws.CloseAll("server shutting down")
		if werr := a.ws.Wait(sctx); werr != nil {
			logger.L.Warn("WS: shutdown wait timed out", "conns", a.ws.Len())
		}
		a.connsClosed() // only now: ops applied while clients disconnected reach the persist writer

		if err != nil {
			return fmt.Errorf("app: shutdown: %w", err)
		}
		return nil
	})

	err := g.Wait()
	logger.L.Info("Config: stopped", "instance", a.instanceID)
	return err
}
