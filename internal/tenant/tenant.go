// Package tenant is the admission policy (D-024) and the per-node tenancy state every `open`
// consults: which projects are suspended (D-027) and which sheet names were revoked and must never
// be autocreated again (D-025). internal/ws asks it on open; internal/api and the control bus
// change it. Projects and keys themselves live in the api stores (Mem/DB); a Directory reaches
// them only through KeyResolver.
package tenant

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Sentinels. The ws router maps both NotAuthorized and an unknown sheet in key mode to the one
// wire string protocol.ErrMsgNotAuthorized (no oracle, D-024).
var (
	ErrNotAuthorized = errors.New("tenant: not authorized")
	ErrSuspended     = errors.New("tenant: project suspended")
	ErrUnknownKey    = errors.New("tenant: unknown or revoked key") // KeyResolver's not-found
)

// KeyResolver maps a presented key's SHA-256 to the key and its project. A revoked or unknown key
// is ErrUnknownKey. Implemented by internal/api's MemStore and DBStore.
type KeyResolver interface {
	ResolveKey(ctx context.Context, hash []byte) (keyID, project uuid.UUID, err error)
}

// HashKey is an API key's at-rest form: sha256 (D-020).
func HashKey(key string) []byte {
	sum := sha256.Sum256([]byte(key))
	return sum[:]
}

// Directory is one node's tenancy state. map + RWMutex (D-002 house rule); every method is safe
// for concurrent use.
type Directory struct {
	keys    KeyResolver
	keyMode bool
	onAdmit func(keyID uuid.UUID) // D-028 last_used_at; must not block
	mu      sync.RWMutex
	suspend map[uuid.UUID]struct{}
	revoked *fifoSet[string]    // sheet names dropped by a revoke: never autocreated again (D-025, bounded D-041)
	deadKey *fifoSet[uuid.UUID] // key ids revoked on this node (the open-vs-revoke race; bounded D-041)
}

// New builds a Directory. keyMode is REDLITE_ADMISSION=key. keys may be nil in password mode.
// The revoked-name and dead-key sets are capped at DefaultMemoryMax each until SetMemoryMax.
func New(keys KeyResolver, keyMode bool) *Directory {
	return &Directory{keys: keys, keyMode: keyMode, suspend: map[uuid.UUID]struct{}{},
		revoked: newFIFOSet[string]("revoked_sheet_names", DefaultMemoryMax),
		deadKey: newFIFOSet[uuid.UUID]("dead_key_ids", DefaultMemoryMax)}
}

// SetMemoryMax caps each of the revoked-name and dead-key sets at n entries (REDLITE_TENANT_MEMORY_MAX,
// D-041); n ≤ 0 keeps DefaultMemoryMax. A set already above n drops its oldest entries now.
func (d *Directory) SetMemoryMax(n int) {
	if n <= 0 {
		n = DefaultMemoryMax
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.revoked.setCap(n)
	d.deadKey.setCap(n)
}

// Stats is a point-in-time count of the directory's state (counts only, D-038).
type Stats struct {
	Suspended    int    // projects suspended on this node
	RevokedNames int    // revoked sheet names held (≤ the memory max)
	DeadKeys     int    // revoked key ids held (≤ the memory max)
	NamesDropped uint64 // revoked names dropped by the bound since boot
	KeysDropped  uint64 // dead key ids dropped by the bound since boot
}

// Stats counts the directory's sets.
func (d *Directory) Stats() Stats {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return Stats{Suspended: len(d.suspend), RevokedNames: d.revoked.len(), DeadKeys: d.deadKey.len(),
		NamesDropped: d.revoked.dropped, KeysDropped: d.deadKey.dropped}
}

// ResetRevocations empties the revoked-name and dead-key sets. It is for a reload from Postgres
// (persistence on), where the store is the record: a revoked key no longer resolves and a revoked
// project's sheets are deleted. With persistence off nothing calls it (the sets are the only memory
// of a revoke on this node, bounded by the memory max). The drop counters are kept.
func (d *Directory) ResetRevocations() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.revoked.reset()
	d.deadKey.reset()
}

// KeyMode reports REDLITE_ADMISSION=key.
func (d *Directory) KeyMode() bool { return d.keyMode }

// OnAdmit registers fn, called with the key id after every key-mode admission (last_used_at).
// Call before serving.
func (d *Directory) OnAdmit(fn func(keyID uuid.UUID)) { d.onAdmit = fn }

// Admit decides an open of a sheet owned by project (D-024, D-027). Suspended → ErrSuspended (both
// modes). Password mode → uuid.Nil, nil (the sheet password is the caller's check). Key mode →
// apiKey must resolve to a live key of project, else ErrNotAuthorized; returns the key id the
// session is admitted with.
func (d *Directory) Admit(ctx context.Context, project uuid.UUID, apiKey string) (uuid.UUID, error) {
	if !d.keyMode {
		// password mode: the caller checked the sheet password before calling Admit
		if d.Suspended(project) {
			return uuid.Nil, ErrSuspended
		}
		return uuid.Nil, nil
	}
	// key mode: the key is checked BEFORE suspension, so only a holder of a valid key of this
	// project learns that it is suspended (no oracle for anonymous clients, D-024).
	if apiKey == "" || d.keys == nil {
		return uuid.Nil, ErrNotAuthorized
	}
	// bounded: a stalled pool must not hang every open (the caller ctx is the upgrade request)
	lctx, cancel := context.WithTimeout(ctx, resolveTimeout)
	defer cancel()
	keyID, owner, err := d.keys.ResolveKey(lctx, HashKey(apiKey))
	switch {
	case errors.Is(err, ErrUnknownKey):
		return uuid.Nil, ErrNotAuthorized
	case err != nil:
		return uuid.Nil, fmt.Errorf("tenant: admit: %w", err)
	case owner != project, d.KeyRevoked(keyID):
		return uuid.Nil, ErrNotAuthorized
	}
	if d.Suspended(project) {
		return uuid.Nil, ErrSuspended
	}
	if d.onAdmit != nil {
		d.onAdmit(keyID)
	}
	return keyID, nil
}

// SetSuspended marks project suspended or resumed on this node.
func (d *Directory) SetSuspended(project uuid.UUID, suspended bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if suspended {
		d.suspend[project] = struct{}{}
	} else {
		delete(d.suspend, project)
	}
}

// Suspended reports whether project is suspended.
func (d *Directory) Suspended(project uuid.UUID) bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	_, ok := d.suspend[project]
	return ok
}

// ReplaceSuspended sets the whole suspended set (boot load, reconcile).
func (d *Directory) ReplaceSuspended(projects []uuid.UUID) {
	m := make(map[uuid.UUID]struct{}, len(projects))
	for _, p := range projects {
		m[p] = struct{}{}
	}
	d.mu.Lock()
	d.suspend = m
	d.mu.Unlock()
}

// MarkRevoked records sheet names a revoke dropped, so autocreate never recreates them (D-025).
func (d *Directory) MarkRevoked(names ...string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, n := range names {
		d.revoked.add(n)
	}
}

// Revoked reports whether name was dropped by a revoke on this node and is still held (the set
// keeps the newest memory-max names, D-041).
func (d *Directory) Revoked(name string) bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.revoked.has(name)
}

// Event is one audit_events row (D-026, 000002). Detail holds identifiers only — never a key,
// hash, password or cached value; the auditor drops nothing, so callers must not put secrets in.
type Event struct {
	Project uuid.UUID         // uuid.Nil for a cluster event
	Actor   string            // "admin", "key:<prefix>", "instance:<id>", "session:<id>"
	Action  string            // session_open, session_close, session_kick, project_suspended, key_revoked, …
	Subject string            // the session id, project id, key id or sheet name acted on
	Detail  map[string]string // identifiers only
}

// Auditor records Events. It must not block (internal/persist's implementation is a bounded,
// non-blocking queue; NopAuditor is used when persistence is off).
type Auditor interface {
	Audit(Event)
}

// NopAuditor records nothing (persistence off: the log line at the call site is the record).
type NopAuditor struct{}

// Audit does nothing.
func (NopAuditor) Audit(Event) {}

// CheckBearer reports whether r carries `Authorization: Bearer <token>`, compared in constant
// time (D-031 cluster token; internal/replica on /replica, internal/app on /health). An empty
// token disables the check (development only — production refuses to start without one).
func CheckBearer(r *http.Request, token string) bool {
	if token == "" {
		return true
	}
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}

// resolveTimeout bounds the key lookup on open (a DB round trip with persistence on).
const resolveTimeout = 2 * time.Second

// MarkKeyRevoked records that keyID was revoked (or rotated out) on this node. Admit refuses it
// even if the store still answers for it, and KeyRevoked lets an open that raced the revoke
// close its just-added session (D-028, criterion 7).
func (d *Directory) MarkKeyRevoked(keyID uuid.UUID) {
	d.mu.Lock()
	d.deadKey.add(keyID)
	d.mu.Unlock()
}

// KeyRevoked reports whether keyID was revoked on this node and is still held (newest memory-max
// ids, D-041).
func (d *Directory) KeyRevoked(keyID uuid.UUID) bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.deadKey.has(keyID)
}
