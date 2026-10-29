package reader

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/CrimsonCoder42/red_lite_cache/protocol"
)

// Errors a caller matches with errors.Is. ErrNotConnected, ErrAuthFailed and ErrClosed keep the v1
// names (advo_cache/reader/reader.go:47-51); backendAdvo checks errors.Is(err, reader.ErrClosed).
var (
	ErrNotConnected = errors.New("reader: not connected to server")
	ErrClosed       = errors.New("reader: closed")
	ErrAuthFailed   = errors.New("reader: authentication failed")
	// ErrStale: the server refused a set because an invalidation covering the key was applied after
	// the read the value came from (R6). The value is not stored; the caller simply does not cache it.
	ErrStale = errors.New("reader: stale write")
	// ErrTimeout: no acknowledgement within Config.WriteTimeout or the caller's context.
	ErrTimeout = errors.New("reader: timeout")
)

// Defaults for zero Config fields.
const (
	DefaultConnectTimeout    = 5 * time.Second
	DefaultWriteTimeout      = 5 * time.Second
	DefaultMaxEntries        = 100_000
	DefaultMaxPendingDeletes = 10_000
	DefaultReconnectMin      = 100 * time.Millisecond
	DefaultReconnectMax      = 30 * time.Second
	DefaultMissWindow        = 30 * time.Second
)

// missSeqMax bounds the per-key miss record behind base_seq (R6).
const missSeqMax = 65_536

// Config is the reader's only input. LeaderAddr, BackupAddr, Sheet and Password keep their v1 names
// (advo_cache/reader/reader.go:58-67); every other field is optional.
type Config struct {
	LeaderAddr string // WebSocket URL of the leader, e.g. "ws://cache:8081/ws"
	BackupAddr string // optional: the replica, tried alternately with the leader
	Sheet      string
	Password   string

	Context context.Context // optional: the reader's lifetime; cancelling it is Close (nil = Background)
	Logger  *slog.Logger    // optional (nil = slog.Default())

	// ConnectTimeout is how long New waits for the first connect; after it New returns the reader
	// anyway, still reconnecting (R10). It is also the per-attempt handshake deadline.
	ConnectTimeout time.Duration
	// WriteTimeout bounds each write and the wait for its acknowledgement.
	WriteTimeout time.Duration
	// MaxEntries bounds the local map (R4, D-008).
	MaxEntries int
	// MaxPendingDeletes bounds the replay queue; overflow collapses it to one delete_prefix "" (D-005).
	MaxPendingDeletes int
	// ReconnectMin and ReconnectMax bound the reconnect backoff (full jitter, forever) (R1, D-004).
	ReconnectMin, ReconnectMax time.Duration
	// MissWindow is how long a key's Get/Has miss stays live as the base_seq of sets of that key
	// (R6): concurrent read-throughs of one key all send the oldest live miss (one is kept per
	// MissWindow/2, so any read-through faster than MissWindow/2 is covered), and a set never
	// consumes it. Consequence: after a write covering a key, read-through sets of that key are refused
	// (ErrStale) for up to MissWindow — never stale. It should match the server's
	// REDLITE_SWEEP_LOG_WINDOW (default 30s): past the server window a base is below the sweep-log
	// floor and refused anyway. 0 = DefaultMissWindow.
	MissWindow time.Duration
}

func (c Config) withDefaults() Config {
	if c.Context == nil {
		c.Context = context.Background()
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.ConnectTimeout <= 0 {
		c.ConnectTimeout = DefaultConnectTimeout
	}
	if c.WriteTimeout <= 0 {
		c.WriteTimeout = DefaultWriteTimeout
	}
	if c.MaxEntries <= 0 {
		c.MaxEntries = DefaultMaxEntries
	}
	if c.MaxPendingDeletes <= 0 {
		c.MaxPendingDeletes = DefaultMaxPendingDeletes
	}
	if c.ReconnectMin <= 0 {
		c.ReconnectMin = DefaultReconnectMin
	}
	if c.ReconnectMax <= 0 {
		c.ReconnectMax = DefaultReconnectMax
	}
	if c.MissWindow <= 0 {
		c.MissWindow = DefaultMissWindow
	}
	c.ReconnectMax = max(c.ReconnectMax, c.ReconnectMin)
	return c
}

// Reader is an embeddable cache client: O(1) local reads, acknowledged writes, a connection kept
// alive in the background.
type Reader struct {
	cfg    Config
	log    *slog.Logger
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	local *local

	closed    atomic.Bool
	connected atomic.Bool   // written under cmu; read lock-free by Get/Has
	epoch     atomic.Uint64 // highest leader epoch seen (D-012)
	nextID    atomic.Uint64

	cmu   sync.Mutex // guards sess, queue, every session's pending/dead, and connected transitions
	sess  *session
	queue []op // deletes awaiting replay, oldest first

	firstOnce sync.Once
	first     chan error // the first connect outcome, for New
}

// New starts a reader and waits up to ConnectTimeout for the first connect. It returns the reader
// even when the cache is down (still reconnecting, IsConnected false, reads miss) and fails only on
// a refused password (ErrAuthFailed) or a config without an address or sheet.
//
// after advo_cache/reader/reader.go:121-148: fail when leader and backup both refuse the first
// dial → return after ConnectTimeout and keep reconnecting (R10).
func New(cfg Config) (*Reader, error) {
	cfg = cfg.withDefaults()
	if cfg.LeaderAddr == "" && cfg.BackupAddr == "" {
		return nil, errors.New("reader: LeaderAddr or BackupAddr required")
	}
	if cfg.Sheet == "" {
		return nil, errors.New("reader: Sheet required")
	}
	ctx, cancel := context.WithCancel(cfg.Context)
	r := &Reader{
		cfg:    cfg,
		log:    cfg.Logger.With("sheet", cfg.Sheet),
		ctx:    ctx,
		cancel: cancel,
		local:  newLocal(cfg.MaxEntries, missSeqMax, cfg.MissWindow),
		first:  make(chan error, 1),
	}
	r.wg.Go(r.run)

	t := time.NewTimer(cfg.ConnectTimeout)
	defer t.Stop()
	select {
	case err := <-r.first:
		if err != nil {
			_ = r.Close()
			return nil, err
		}
	case <-t.C:
		r.log.Warn("Reader: started disconnected", "leader", cfg.LeaderAddr, "backup", cfg.BackupAddr, "connect_timeout", cfg.ConnectTimeout)
	case <-ctx.Done():
		_ = r.Close()
		return nil, fmt.Errorf("reader: new: %w", ctx.Err())
	}
	return r, nil
}

func (r *Reader) signalFirst(err error) {
	r.firstOnce.Do(func() { r.first <- err })
}

func (r *Reader) isClosed() bool { return r.closed.Load() || r.ctx.Err() != nil }

// entry returns the live local entry for key, or records a miss (R6). Disconnected = miss (R3).
func (r *Reader) entry(key string) (localEntry, bool) {
	if r.connected.Load() && !r.closed.Load() {
		if e, ok := r.local.lookup(key, time.Now()); ok {
			return e, true
		}
	}
	r.local.miss(key, time.Now())
	return localEntry{}, false
}

// Get decodes the value of key into target (a pointer) and reports a hit. A miss is: disconnected
// (R3), absent, locally expired (R4), the null placeholder, or a value that does not decode into
// target. Each call decodes the stored bytes afresh, so the caller owns what it gets (R8).
//
// after advo_cache/reader/reader.go:157-159: returned the shared decoded value (map[string]any),
// so backendAdvo marshalled and unmarshalled per hit (store.go:166-178) → one decode into target.
func (r *Reader) Get(key string, target any) bool {
	e, ok := r.entry(key)
	if !ok || e.isNull() {
		return false
	}
	return json.Unmarshal(e.raw, target) == nil
}

// GetRaw returns the value decoded into the v1 shape (map[string]any, []any, string, float64,
// bool), freshly decoded per call. The null placeholder is returned as the string "__NULL__", as
// in v1.
func (r *Reader) GetRaw(key string) (any, bool) {
	e, ok := r.entry(key)
	if !ok {
		return nil, false
	}
	var v any
	if json.Unmarshal(e.raw, &v) != nil {
		return nil, false
	}
	return v, true
}

// Has reports whether key holds a live value (the null placeholder included, as in v1). False
// while disconnected.
func (r *Reader) Has(key string) bool {
	_, ok := r.entry(key)
	return ok
}

// IsNull reports whether key holds the set_null placeholder. False while disconnected.
func (r *Reader) IsNull(key string) bool {
	if !r.connected.Load() {
		return false
	}
	e, ok := r.local.lookup(key, time.Now())
	return ok && e.isNull()
}

// Set is SetWithTTL with the server's default TTL.
func (r *Reader) Set(ctx context.Context, key string, value any) error {
	return r.SetWithTTL(ctx, key, value, 0)
}

// SetWithTTL sends a set and waits for its acknowledgement (R9). On success the value is in the
// local map with the server's remaining lifetime — applied by the read loop in the server's order
// relative to broadcasts. The set carries base_seq: the sheet seq at this reader's oldest miss of
// key within MissWindow, else the applied seq (R6). It returns ErrStale when the server refused it as stale, ErrNotConnected when there is no
// connection, ErrTimeout without an ack, ErrClosed after Close; in every failure nothing is stored.
// ttlMinutes <= 0 uses the server default.
//
// after advo_cache/reader/reader.go:213-237: store locally first, then fire-and-forget → ack first,
// store on ack, base_seq added.
func (r *Reader) SetWithTTL(ctx context.Context, key string, value any, ttlMinutes int) error {
	if r.isClosed() {
		return ErrClosed
	}
	if key == "" {
		return errors.New("reader: set: key required")
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("reader: set %q: encode: %w", key, err)
	}
	if !r.connected.Load() {
		return fmt.Errorf("reader: set %q: %w", key, ErrNotConnected)
	}
	o := op{kind: opSet, arg: key, value: raw, ttl: max(ttlMinutes, 0), baseSeq: r.local.baseSeq(key, time.Now())}
	c, err := r.send(o, false)
	if err != nil {
		if errors.Is(err, ErrNotConnected) || errors.Is(err, ErrTimeout) {
			return fmt.Errorf("reader: set %q: %w", key, err)
		}
		return fmt.Errorf("reader: set %q: %w: %w", key, ErrNotConnected, err)
	}
	resp, err := r.await(ctx, c)
	switch {
	case errors.Is(err, errLinkLost):
		return fmt.Errorf("reader: set %q: %w", key, ErrNotConnected)
	case err != nil:
		return fmt.Errorf("reader: set %q: %w", key, err)
	case resp.Success:
		return nil
	case resp.Error == protocol.ErrMsgStaleWrite:
		return fmt.Errorf("reader: set %q: %w", key, ErrStale)
	default:
		return fmt.Errorf("reader: set %q: server: %s", key, resp.Error)
	}
}

// Delete removes key. See deleteOp for the guarantee.
func (r *Reader) Delete(ctx context.Context, key string) error {
	if key == "" {
		if r.isClosed() {
			return ErrClosed
		}
		return errors.New("reader: delete: key required")
	}
	return r.deleteOp(ctx, op{kind: opDelete, arg: key})
}

// DeletePrefix removes every key starting with prefix ("" = every key).
func (r *Reader) DeletePrefix(ctx context.Context, prefix string) error {
	return r.deleteOp(ctx, op{kind: opDeletePrefix, arg: prefix})
}

// DeleteSuffix removes every key ending with suffix.
func (r *Reader) DeleteSuffix(ctx context.Context, suffix string) error {
	return r.deleteOp(ctx, op{kind: opDeleteSuffix, arg: suffix})
}

// deleteOp applies a delete locally first, then, when connected, sends it and waits for the ack.
// When it cannot be sent or acknowledged — not connected, write failed, timeout, link lost — it is
// queued and replayed, in order, before the reader is next connected; the call returns nil (R2,
// D-005). It returns an error only after Close (ErrClosed) or when the server answered with an
// error.
//
// after advo_cache/reader/reader.go:242-321: ErrNotConnected before touching the map → local first,
// queue and replay.
func (r *Reader) deleteOp(ctx context.Context, o op) error {
	if r.isClosed() {
		return ErrClosed
	}
	r.local.remove(o.kind, o.arg, 0)
	c, err := r.send(o, true)
	if err != nil {
		// errQueued, or a write error that leaves the call pending for teardown to requeue.
		return nil
	}
	resp, err := r.await(ctx, c)
	switch {
	case errors.Is(err, ErrClosed):
		return ErrClosed
	case errors.Is(err, ErrTimeout) && ctx.Err() == nil:
		// The link is not answering: end it; teardown requeues the pending delete.
		c.sess.cancel()
		return nil
	case err != nil:
		// Link lost (requeued by teardown) or the caller gave up (still in flight, requeued if the
		// link dies before the ack).
		return nil
	case resp.Success:
		return nil
	default:
		return fmt.Errorf("reader: %s %q: server: %s", o.action(), o.arg, resp.Error)
	}
}

// IsConnected is true only after the sheet_state is loaded and every queued delete is acked.
func (r *Reader) IsConnected() bool { return r.connected.Load() && !r.isClosed() }

// Close stops the reader and waits for its goroutines. It is idempotent; every later call except
// Close returns ErrClosed (reads miss).
func (r *Reader) Close() error {
	r.closed.Store(true)
	r.cancel()
	r.wg.Wait()
	return nil
}
