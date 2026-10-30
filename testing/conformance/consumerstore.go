// Covers: backendAdvo cache layer
// Package conformance replays how backendAdvo uses its cache layer against the real reader and a
// real leader (D-032). This file is a faithful MINIATURE of backendAdvo/cache: the read-through
// helpers, the one profile sweep, the key helpers and five TTL constants, over the PUBLIC reader
// API only (never internal/). The loader is FakeDB, which counts loads so a test can prove how
// many times the "database" was hit.
package conformance

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/CrimsonCoder42/red_lite_cache/reader"
)

// TTL constants, in MINUTES like backendAdvo's (TTLMinutes = int).
// after backendAdvo/cache/keys.go:2553 (TTLAnalytics), :2574 (TTLVitals), :606
// (TTLEmergencyContacts), :2442 (TTLNutritionCatalog), :2707 (TTLTenant): same values.
const (
	TTLAnalytics         = 5
	TTLVitals            = 15
	TTLEmergencyContacts = 30
	TTLNutritionCatalog  = 60
	TTLTenant            = 60
)

// ErrEmptyPrefix is the consumer refusing a sweep that would be a whole-keyspace delete (an empty
// root or a nil profile). backendAdvo never sends one: invalidateDomainBoth skips uuid.Nil
// (store.go:293-300) and every root is a non-empty const.
var ErrEmptyPrefix = errors.New("conformance: refusing an empty sweep prefix")

// DomainPatientPrefix is "<root>:patient:<uuid>", the head every profile-scoped key starts with.
// after backendAdvo/cache/keys.go:60-62 (domainPatientPrefix): identical format.
func DomainPatientPrefix(root string, patientID uuid.UUID) string {
	return fmt.Sprintf("%s:patient:%s", root, patientID.String())
}

// DomainFeedWindowKey is "<prefix>:feed:<startNanos>:<endNanos>".
// after backendAdvo/cache/keys.go:127-129 (domainFeedWindowKey): identical format.
func DomainFeedWindowKey(prefix string, start, end time.Time) string {
	return fmt.Sprintf("%s:feed:%d:%d", prefix, start.UnixNano(), end.UnixNano())
}

// DomainCursorKey renders a keyset cursor for a key: "<nanos>:<id>", "0" / "none" when absent.
// after backendAdvo/cache/keys.go:81-102 (domainCursorNanos + domainCursorKey + domainUUIDString):
// pgtype.Timestamptz/pgtype.UUID replaced by time.Time/uuid.UUID (zero = absent), same spelling.
func DomainCursorKey(cursor time.Time, cursorID uuid.UUID) string {
	nanos := int64(0)
	if !cursor.IsZero() {
		nanos = cursor.UnixNano()
	}
	id := "none"
	if cursorID != uuid.Nil {
		id = cursorID.String()
	}
	return fmt.Sprintf("%d:%s", nanos, id)
}

// EncodeCursor is the API's opaque page cursor: base64url of "<unix-nanos>:<uuid>". Clients send
// it back verbatim; the cache key uses DomainCursorKey of the decoded pair.
func EncodeCursor(t time.Time, id uuid.UUID) string {
	return base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("%d:%s", t.UnixNano(), id)))
}

// Store is one backendAdvo instance's cache.Store: a reader and nothing else.
// after backendAdvo/cache/store.go:99-104 (Store wraps the reader); the db half is FakeDB.
type Store struct {
	R *reader.Reader
}

// CachedOne is the read-through for one value: hit → return it; miss → load → SetWithTTL.
// Errors are never cached; a zero value that loaded successfully is.
// after backendAdvo/cache/shared.go:146-157 (cachedOne): s.Get(key,&cached) is now the reader's
// typed Get — one decode, no marshal/unmarshal round trip (store.go:166-178 is what it replaces).
func CachedOne[T any](s *Store, ctx context.Context, key string, ttlMinutes int, load func() (T, error)) (T, error) {
	var cached T
	if s.R.Get(key, &cached) {
		return cached, nil
	}
	loaded, err := load()
	if err != nil {
		return loaded, err
	}
	_ = s.R.SetWithTTL(ctx, key, loaded, ttlMinutes) // ErrStale = a sweep raced this load: not cached
	return loaded, nil
}

// CachedList is CachedOne for a slice.
// after backendAdvo/cache/shared.go:109-120 (cachedList): same shape.
func CachedList[T any](s *Store, ctx context.Context, key string, ttlMinutes int, load func() ([]T, error)) ([]T, error) {
	var cached []T
	if s.R.Get(key, &cached) {
		return cached, nil
	}
	points, err := load()
	if err != nil {
		return nil, err
	}
	_ = s.R.SetWithTTL(ctx, key, points, ttlMinutes)
	return points, nil
}

// InvalidateDomain drops every cached view a domain holds for one profile: ONE DeletePrefix on
// "<root>:patient:<uuid>".
// after backendAdvo/cache/store.go:269-275 (invalidateDomain): WITHOUT the second sweep
// secondSweepDelay later (G-01-16) — the reader's base_seq makes a racing read-through's Set
// refused instead of re-cached (D-023), which C2/C3 prove. An empty prefix is refused here.
func (s *Store) InvalidateDomain(ctx context.Context, root string, patientID uuid.UUID) error {
	if root == "" || patientID == uuid.Nil {
		return ErrEmptyPrefix
	}
	return s.R.DeletePrefix(ctx, DomainPatientPrefix(root, patientID))
}

// InvalidateDomainBoth sweeps the owner's and, when different and known, the requester's head.
// after backendAdvo/cache/store.go:293-298 (invalidateDomainBoth): uuid.Nil requester skipped; a
// failed owner sweep does not stop the requester sweep (backendAdvo logs and sweeps both) — both
// errors are returned.
func (s *Store) InvalidateDomainBoth(ctx context.Context, root string, ownerID, requesterID uuid.UUID) error {
	err := s.InvalidateDomain(ctx, root, ownerID)
	if requesterID != uuid.Nil && requesterID != ownerID {
		err = errors.Join(err, s.InvalidateDomain(ctx, root, requesterID))
	}
	return err
}

// Row is one fake database row.
type Row struct {
	ID      uuid.UUID `json:"id"`
	Profile uuid.UUID `json:"profile"`
	Value   int       `json:"value"`
}

// FakeDB is the database every instance reads through: rows per profile and a load counter.
// LoadHook, when set, runs inside every load after the rows are read and before they are
// returned — a test parks a load there (C3: "mid-load of the OLD row").
type FakeDB struct {
	mu       sync.Mutex
	rows     map[uuid.UUID][]Row
	loads    atomic.Int64
	fail     atomic.Bool
	LoadHook func(profile uuid.UUID)
}

// NewFakeDB returns an empty database.
func NewFakeDB() *FakeDB { return &FakeDB{rows: map[uuid.UUID][]Row{}} }

// ErrDB is what a failing load returns (C10).
var ErrDB = errors.New("conformance: fake database error")

// Put replaces profile's rows (a write).
func (d *FakeDB) Put(profile uuid.UUID, rows ...Row) {
	d.mu.Lock()
	d.rows[profile] = append([]Row(nil), rows...)
	d.mu.Unlock()
}

// Fail makes every load return ErrDB until Fail(false).
func (d *FakeDB) Fail(on bool) { d.fail.Store(on) }

// Loads is how many loads ran (successful or not).
func (d *FakeDB) Loads() int64 { return d.loads.Load() }

// Load returns a copy of profile's rows; it is the loader a CachedList call passes.
func (d *FakeDB) Load(profile uuid.UUID) ([]Row, error) {
	d.loads.Add(1)
	if d.fail.Load() {
		return nil, ErrDB
	}
	d.mu.Lock()
	rows := append([]Row(nil), d.rows[profile]...)
	d.mu.Unlock()
	if d.LoadHook != nil {
		d.LoadHook(profile)
	}
	return rows, nil
}
