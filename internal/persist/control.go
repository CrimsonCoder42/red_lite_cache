package persist

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/CrimsonCoder42/red_lite_cache/db"
	"github.com/CrimsonCoder42/red_lite_cache/internal/logger"
)

// ControlChannel is the NOTIFY channel of the control bus (D-021).
const ControlChannel = "redlite_control"

// KindProjectRevoked: every key of Project was revoked and its sheets deleted from Postgres.
const KindProjectRevoked = "project_revoked"

// Tenancy kinds (D-027, D-028, D-030): the same names as the leader → replica control frames
// (protocol.Type*), so one apply serves both paths.
const (
	KindProjectSuspended = "project_suspended" // Project suspended: close its sessions 4001, refuse opens
	KindProjectResumed   = "project_resumed"   // Project resumed
	KindKeyRevoked       = "key_revoked"       // KeyID revoked (or rotated out): close its sessions 4002
	KindSessionKill      = "session_kill"      // Session killed by an admin: close it 4003 if held here
)

// Control is one control-bus payload. A reference only (kind + id), never data: the receiver
// re-reads or applies what the row change means (D-021). Not a WebSocket frame; it travels only
// through Postgres between instances of this service.
type Control struct {
	Kind    string `json:"kind"`
	Project string `json:"project,omitempty"`
	KeyID   string `json:"key_id,omitempty"`
	Session string `json:"session,omitempty"`
}

// Notify publishes c on ControlChannel (pg_notify, after the caller's commit).
func Notify(ctx context.Context, store *db.Store, c Control) error {
	b, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("persist: notify: %w", err)
	}
	if _, err := store.Pool().Exec(ctx, "SELECT pg_notify($1, $2)", ControlChannel, string(b)); err != nil {
		return fmt.Errorf("persist: notify %s: %w", c.Kind, err)
	}
	return nil
}

// dedicatedConn takes one connection out of the pool for good (LISTEN, an advisory lock): its
// session state must not leak to pool users, and its death must be observable.
func dedicatedConn(ctx context.Context, store *db.Store) (*pgx.Conn, error) {
	pc, err := store.Pool().Acquire(ctx)
	if err != nil {
		return nil, err
	}
	return pc.Hijack(), nil
}

// Control-listener reconnect backoff: full jitter between 100ms and a cap doubling to 10s.
const (
	listenBackoffMin = 100 * time.Millisecond
	listenBackoffMax = 10 * time.Second
)

// RunControlListener LISTENs on ControlChannel on one dedicated connection and calls apply for
// every well-formed payload, in order, until ctx ends (returns nil). A lost connection is
// re-established with backoff; notifications sent meanwhile are lost (NOTIFY is not durable), which
// the cleaner-tick reconcile covers.
//
// after code-heim go_74_pgx (doc/reference/codeheim_patterns.md § Tier 3): the pool + Exec shape,
// plus a hijacked connection for LISTEN and WaitForNotification on ctx.
func RunControlListener(ctx context.Context, store *db.Store, apply func(Control)) error {
	ceil := listenBackoffMin
	for ctx.Err() == nil {
		err := listenOnce(ctx, store, apply, func() { ceil = listenBackoffMin })
		if ctx.Err() != nil {
			return nil
		}
		logger.L.Warn("Persist: control listener lost", "err", err, "retry_max_ms", ceil.Milliseconds())
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(rand.N(ceil) + time.Millisecond):
		}
		ceil = min(ceil*2, listenBackoffMax)
	}
	return nil
}

func listenOnce(ctx context.Context, store *db.Store, apply func(Control), listening func()) error {
	conn, err := dedicatedConn(ctx, store)
	if err != nil {
		return fmt.Errorf("persist: control: acquire: %w", err)
	}
	defer func() { // bounded like the elector's close: a half-open socket must not hang shutdown
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		defer cancel()
		conn.Close(cctx)
	}()
	if _, err := conn.Exec(ctx, "LISTEN "+ControlChannel); err != nil {
		return fmt.Errorf("persist: control: listen: %w", err)
	}
	listening()
	logger.L.Info("Persist: control listening", "channel", ControlChannel)
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return err
			}
			return fmt.Errorf("persist: control: wait: %w", err)
		}
		var c Control
		if err := json.Unmarshal([]byte(n.Payload), &c); err != nil || c.Kind == "" {
			logger.L.Warn("Persist: control payload ignored", "channel", n.Channel, "err", err)
			continue
		}
		logger.L.Info("Persist: control received", "kind", c.Kind, "project", c.Project, "key_id", c.KeyID, "session", c.Session)
		apply(c)
	}
}

// SheetNames is every sheet name in Postgres (the reconcile's view of the registry).
func SheetNames(ctx context.Context, store *db.Store) (map[string]bool, error) {
	rows, err := store.Registry.ListSheets(ctx)
	if err != nil {
		return nil, fmt.Errorf("persist: list sheets: %w", err)
	}
	out := make(map[string]bool, len(rows))
	for _, r := range rows {
		out[r.Name] = true
	}
	return out, nil
}

// Reconciler is the cleaner-tick fallback of the control bus: it removes from memory every sheet
// that Postgres had at the previous pass and no longer has (a missed project_revoked). A sheet
// never seen in Postgres is left alone: a leader's new sheet may not be flushed yet. The first pass
// only records. Not safe for concurrent Run calls (one cleaner per process).
type Reconciler struct {
	store    *db.Store
	drop     func(name string)
	lastSeen map[string]bool
}

// NewReconciler builds a reconciler that calls drop for each sheet that disappeared.
func NewReconciler(store *db.Store, drop func(name string)) *Reconciler {
	return &Reconciler{store: store, drop: drop}
}

// Pass runs one reconcile.
func (r *Reconciler) Pass(ctx context.Context) {
	names, err := SheetNames(ctx, r.store)
	if err != nil {
		if ctx.Err() == nil {
			logger.L.Error("Persist: reconcile failed", "err", err)
		}
		return
	}
	dropped := 0
	for name := range r.lastSeen {
		if !names[name] {
			r.drop(name)
			dropped++
		}
	}
	r.lastSeen = names
	if dropped > 0 {
		logger.L.Info("Persist: reconciled", "dropped", dropped, "sheets", len(names))
	}
}
