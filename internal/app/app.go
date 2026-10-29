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
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/CrimsonCoder42/red_lite_cache/internal/api"
	"github.com/CrimsonCoder42/red_lite_cache/internal/cache"
	"github.com/CrimsonCoder42/red_lite_cache/internal/config"
	"github.com/CrimsonCoder42/red_lite_cache/internal/lock"
	"github.com/CrimsonCoder42/red_lite_cache/internal/logger"
	"github.com/CrimsonCoder42/red_lite_cache/internal/sheet"
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
}

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
		}),
		locks: lock.NewManager(nil),
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

	a.ws = ws.NewHandler(a.reg, a.locks, a.node, ws.Options{
		SendQueue:     cfg.ConnSendQueue,
		MaxFrameBytes: cfg.MaxFrameBytes,
		PingInterval:  cfg.PingInterval,
		WriteTimeout:  cfg.WriteTimeout,
		LockTTL:       cfg.LockTTL,
	})
	a.mux.HandleFunc("GET "+protocol.PathHealth, a.health)
	a.mux.Handle(protocol.PathWS, a.ws)

	// S6 management API: projects and keys in PostgreSQL when persistence is on, else in memory.
	var projects api.Store = api.NewMemStore()
	if store := a.Store(); store != nil {
		projects = api.NewDBStore(store)
	}
	adm := api.New(a.reg, projects, cfg.AdminKey, a.node.IsLeader)
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
func (a *App) health(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(protocol.Health{Status: "ok", Role: a.role(), Epoch: a.node.Epoch(), InstanceID: a.instanceID})
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
