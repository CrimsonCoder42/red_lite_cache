package persist

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/CrimsonCoder42/red_lite_cache/db"
	"github.com/CrimsonCoder42/red_lite_cache/internal/logger"
)

// LeaderLockKey is the advisory lock that decides the leader in REDLITE_ELECTION=advisory (D-022).
const LeaderLockKey = "redlite-leader"

// electorReq is one request to the elector's owner goroutine.
type electorReq struct {
	acquire bool // true: try to take the lock; false: release it
	reply   chan bool
}

// Elector holds the leader advisory lock on one dedicated connection. One goroutine (Run) owns
// the connection; TryAcquire and Release are requests to it (confinement: a pgx.Conn is not safe
// for concurrent use, and no mutex is held across the network). While held, Run pings the
// connection every interval; a failed ping means the session — and so the lock — may be gone, and
// onLost is called: the node can no longer prove leadership and steps down.
//
// after code-heim go_20_mutexes_and_confinement (doc/reference/codeheim_patterns.md § Tier 2):
// the owner goroutine + request channel, with a ctx and a reply channel per request.
type Elector struct {
	store *db.Store
	reqs  chan electorReq
}

// NewElector builds an elector on store's pool. Run must be running for TryAcquire/Release.
func NewElector(store *db.Store) *Elector {
	return &Elector{store: store, reqs: make(chan electorReq)}
}

// TryAcquire asks for the lock once (pg_try_advisory_lock); true when this node holds it now.
// It returns false when ctx ends or Run is not serving.
func (e *Elector) TryAcquire(ctx context.Context) bool { return e.ask(ctx, true) }

// Release drops the lock if held (step-down by the peer probe).
func (e *Elector) Release(ctx context.Context) { e.ask(ctx, false) }

func (e *Elector) ask(ctx context.Context, acquire bool) bool {
	r := electorReq{acquire: acquire, reply: make(chan bool, 1)}
	select {
	case e.reqs <- r:
	case <-ctx.Done():
		return false
	}
	select {
	case ok := <-r.reply:
		return ok
	case <-ctx.Done():
		return false
	}
}

// Run owns the connection until ctx ends; closing it on exit releases the lock.
func (e *Elector) Run(ctx context.Context, interval time.Duration, onLost func()) error {
	var conn *pgx.Conn
	held := false
	closeConn := func() {
		if conn != nil {
			cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
			conn.Close(cctx)
			cancel()
			conn = nil
		}
		held = false
	}
	defer closeConn()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case r := <-e.reqs:
			if r.acquire {
				ok, err := e.tryLock(ctx, &conn, held)
				if err != nil {
					logger.L.Warn("Persist: advisory lock attempt failed", "err", err)
					closeConn()
				}
				held = ok
			} else if held && conn != nil {
				if _, err := conn.Exec(ctx, "SELECT pg_advisory_unlock(hashtext($1))", LeaderLockKey); err != nil {
					closeConn()
				}
				held = false
				logger.L.Info("Persist: advisory lock released")
			}
			r.reply <- held
		case <-t.C:
			if !held {
				continue
			}
			pctx, cancel := context.WithTimeout(ctx, interval)
			err := conn.Ping(pctx)
			cancel()
			if err != nil && ctx.Err() == nil {
				logger.L.Error("Persist: advisory lock lost", "err", err)
				closeConn()
				onLost()
			}
		}
	}
}

// tryLock opens the dedicated connection if needed and tries the lock (idempotent when held).
func (e *Elector) tryLock(ctx context.Context, conn **pgx.Conn, held bool) (bool, error) {
	if held {
		return true, nil
	}
	if *conn == nil {
		c, err := dedicatedConn(ctx, e.store)
		if err != nil {
			return false, fmt.Errorf("persist: elector: acquire conn: %w", err)
		}
		*conn = c
	}
	var ok bool
	if err := (*conn).QueryRow(ctx, "SELECT pg_try_advisory_lock(hashtext($1))", LeaderLockKey).Scan(&ok); err != nil {
		return false, fmt.Errorf("persist: elector: try lock: %w", err)
	}
	if ok {
		logger.L.Info("Persist: advisory lock acquired")
	}
	return ok, nil
}
