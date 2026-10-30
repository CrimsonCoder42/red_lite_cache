// Package v1compat is advo_cache's reader surface over red_lite_cache's reader (D-035): the same
// Config, constructor, methods and error values as advo_cache/reader/reader.go:47-370, so
// backendAdvo's cut-over step 1 is an import-path change only. Every method is a thin call into
// reader.Reader; nothing here keeps state of its own except the Config it was built from.
//
// Where v1 and this package differ (each one is a v1 defect red_lite_cache fixes, R1-R12):
//
//   - New returns a reader even when no server answers (R10): it waits up to
//     reader.DefaultConnectTimeout, then returns a disconnected reader that keeps reconnecting.
//     v1 returned the dial error when leader and backup both refused (reader.go:121-148). New still
//     fails on a refused open (ErrAuthFailed) as v1 did (reader.go:413-415).
//   - MaxReconnectAttempts is accepted and ignored: the reader reconnects forever (R1). v1 tried 3
//     times per server and gave up (reader.go:544-584).
//   - ReconnectDelay > 0 becomes reader.Config.ReconnectMin (the first backoff step, doubled with
//     full jitter up to reader.DefaultReconnectMax); 0 keeps reader.DefaultReconnectMin (100ms).
//     v1 defaulted to 1s and grew it linearly (reader.go:126-128, 548).
//   - Reads miss while disconnected (R3) and after Close. v1 served its last copy either way.
//   - Get returns a fresh decode per call (R8): mutating the returned map does not change the
//     next Get. v1 returned the one shared decoded value (reader.go:157-159).
//   - Set/SetWithTTL wait for the server's ack and store the value on ack (R9). A set the server
//     refuses as stale (reader.ErrStale, R6) returns nil, as v1's fire-and-forget returned nil once
//     the frame was written (reader.go:213-237): the value is simply not cached. ErrNotConnected,
//     ErrClosed, reader.ErrTimeout and server errors are returned wrapped (errors.Is works).
//   - Delete/DeletePrefix/DeleteSuffix while disconnected are applied locally, queued and replayed
//     before the reader is next connected, and return nil (R2, D-005). v1 returned
//     ErrNotConnected and dropped the delete (reader.go:242-321).
//   - CurrentServer and CacheSize: red_lite_cache's reader exposes neither the address in use nor
//     its local map size, and reader/ is not changed for this package. CurrentServer returns the
//     configured LeaderAddr (BackupAddr when LeaderAddr is ""), not necessarily the address in
//     use; CacheSize returns -1 (unknown). backendAdvo calls neither.
package v1compat

import (
	"context"
	"errors"
	"time"

	"github.com/CrimsonCoder42/red_lite_cache/reader"
)

// The v1 errors, as the SAME values as reader's so errors.Is matches across both packages.
// after advo_cache/reader/reader.go:47-51: same three names, aliased instead of redeclared.
var (
	ErrNotConnected = reader.ErrNotConnected
	ErrAuthFailed   = reader.ErrAuthFailed
	ErrClosed       = reader.ErrClosed
)

// unknownCacheSize is what CacheSize returns: the local map size is not exposed by reader.
const unknownCacheSize = -1

// Config is v1's reader config.
// after advo_cache/reader/reader.go:58-67: same fields; MaxReconnectAttempts is ignored (R1) and
// ReconnectDelay is the first backoff step (see the package doc).
type Config struct {
	LeaderAddr string // WebSocket URL of the leader, e.g. "ws://cache:8095/ws"
	BackupAddr string // optional: the replica, tried alternately with the leader
	Sheet      string
	Password   string

	// MaxReconnectAttempts is accepted for source compatibility and ignored: the reader
	// reconnects forever (R1).
	MaxReconnectAttempts int
	// ReconnectDelay > 0 is the first reconnect backoff step; 0 = reader.DefaultReconnectMin.
	ReconnectDelay time.Duration
}

// readerConfig maps the v1 config onto reader.Config.
func (c Config) readerConfig() reader.Config {
	return reader.Config{
		LeaderAddr:   c.LeaderAddr,
		BackupAddr:   c.BackupAddr,
		Sheet:        c.Sheet,
		Password:     c.Password,
		ReconnectMin: c.ReconnectDelay,
	}
}

// Reader is v1's reader over reader.Reader.
// after advo_cache/reader/reader.go:105-119: the sync.Map, conn and flags are reader.Reader's.
type Reader struct {
	cfg Config
	r   *reader.Reader
}

// New starts a reader. It returns a disconnected, reconnecting reader when no server answers
// within reader.DefaultConnectTimeout (R10), and an error on a refused open (ErrAuthFailed,
// reader.ErrSuspended) or a config without an address or sheet.
// after advo_cache/reader/reader.go:121-148: fail when both dials fail → reader.New (R10).
func New(cfg Config) (*Reader, error) {
	r, err := reader.New(cfg.readerConfig())
	if err != nil {
		return nil, err
	}
	return &Reader{cfg: cfg, r: r}, nil
}

// Get returns the value decoded into the v1 shape (map[string]any, []any, string, float64, bool;
// the null placeholder as the string "__NULL__"), freshly decoded per call. A miss while
// disconnected or closed.
// after advo_cache/reader/reader.go:154-159: the shared stored value → reader.GetRaw.
func (v *Reader) Get(key string) (any, bool) { return v.r.GetRaw(key) }

// GetString returns the value when it is a JSON string; ("", false) when absent or not a string.
// after advo_cache/reader/reader.go:161-170: same type rule.
func (v *Reader) GetString(key string) (string, bool) {
	val, ok := v.r.GetRaw(key)
	if !ok {
		return "", false
	}
	s, ok := val.(string)
	return s, ok
}

// GetMap returns the value when it is a JSON object; (nil, false) when absent or not an object.
// after advo_cache/reader/reader.go:172-181: same type rule.
func (v *Reader) GetMap(key string) (map[string]any, bool) {
	val, ok := v.r.GetRaw(key)
	if !ok {
		return nil, false
	}
	m, ok := val.(map[string]any)
	return m, ok
}

// Has reports whether key holds a live value, the null placeholder included.
// after advo_cache/reader/reader.go:183-187.
func (v *Reader) Has(key string) bool { return v.r.Has(key) }

// IsNull reports whether key holds the set_null placeholder.
// after advo_cache/reader/reader.go:189-197.
func (v *Reader) IsNull(key string) bool { return v.r.IsNull(key) }

// Set is SetWithTTL with the server's default TTL.
// after advo_cache/reader/reader.go:206-208.
func (v *Reader) Set(ctx context.Context, key string, value any) error {
	return v.SetWithTTL(ctx, key, value, 0)
}

// SetWithTTL sets key for ttlMinutes (<= 0 = server default) and waits for the ack. A stale
// refusal (reader.ErrStale) returns nil: v1 returned nil once the frame was sent, and the value is
// not cached either way that matters to a read-through.
// after advo_cache/reader/reader.go:210-237: store locally then fire-and-forget → reader's acked
// set, ErrStale mapped to nil.
func (v *Reader) SetWithTTL(ctx context.Context, key string, value any, ttlMinutes int) error {
	err := v.r.SetWithTTL(ctx, key, value, ttlMinutes)
	if errors.Is(err, reader.ErrStale) {
		return nil
	}
	return err
}

// Delete removes key; queued and replayed when disconnected (returns nil).
// after advo_cache/reader/reader.go:239-264: ErrNotConnected and dropped → queued.
func (v *Reader) Delete(ctx context.Context, key string) error { return v.r.Delete(ctx, key) }

// DeletePrefix removes every key starting with prefix; queued when disconnected.
// after advo_cache/reader/reader.go:266-292.
func (v *Reader) DeletePrefix(ctx context.Context, prefix string) error {
	return v.r.DeletePrefix(ctx, prefix)
}

// DeleteSuffix removes every key ending with suffix; queued when disconnected.
// after advo_cache/reader/reader.go:294-321.
func (v *Reader) DeleteSuffix(ctx context.Context, suffix string) error {
	return v.r.DeleteSuffix(ctx, suffix)
}

// Close stops the reader; idempotent, returns nil. Later writes return ErrClosed; reads miss.
// after advo_cache/reader/reader.go:327-344.
func (v *Reader) Close() error { return v.r.Close() }

// IsConnected reports whether the reader holds a loaded, replayed session.
// after advo_cache/reader/reader.go:346-351.
func (v *Reader) IsConnected() bool { return v.r.IsConnected() }

// CurrentServer returns the configured LeaderAddr (BackupAddr when LeaderAddr is ""). It is an
// approximation: reader does not expose which address the live session uses.
// after advo_cache/reader/reader.go:353-358: the address of the live connection → the configured one.
func (v *Reader) CurrentServer() string {
	if v.cfg.LeaderAddr != "" {
		return v.cfg.LeaderAddr
	}
	return v.cfg.BackupAddr
}

// CacheSize returns -1 (unknown): reader does not expose its local map size.
// after advo_cache/reader/reader.go:360-368: a Range count → unknown.
func (v *Reader) CacheSize() int { return unknownCacheSize }
