package api

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/CrimsonCoder42/red_lite_cache/db"
	"github.com/CrimsonCoder42/red_lite_cache/db/sqlc/registry"
	"github.com/CrimsonCoder42/red_lite_cache/internal/tenant"
)

// ErrNotFound is returned when a project or an API key does not exist (or the key is revoked).
var ErrNotFound = errors.New("api: not found")

// ErrConflict is returned when a project name is taken.
var ErrConflict = errors.New("api: conflict")

// Key is one issued API key as stored: never the key itself, only its hash and prefix.
type Key struct {
	ID         uuid.UUID
	ProjectID  uuid.UUID
	Hash       []byte // sha256 of the key
	Prefix     string // first 8 characters, for logs
	Label      string // operator free text (D-028); never a secret
	CreatedAt  time.Time
	RevokedAt  time.Time // zero = live
	LastUsedAt time.Time // zero = never used
}

// Project is one project row for the listings (D-029). SuspendedAt zero = not suspended. Keys is
// the number of live keys.
type Project struct {
	ID          uuid.UUID
	Name        string
	CreatedAt   time.Time
	SuspendedAt time.Time
	Keys        int
}

// Store holds projects and their API keys.
type Store interface {
	// CreateProject stores the project and its first key in one step.
	CreateProject(ctx context.Context, id uuid.UUID, name string, key Key) error
	// ProjectExists reports whether id is a project.
	ProjectExists(ctx context.Context, id uuid.UUID) (bool, error)
	// ProjectByKeyHash resolves a presented key's hash to its project (ErrNotFound when unknown).
	ProjectByKeyHash(ctx context.Context, hash []byte) (uuid.UUID, error)
	// The store is the admission key resolver too (tenant.KeyResolver, D-024).
	tenant.KeyResolver

	// IssueKey stores another key of key.ProjectID (ErrNotFound: no such project). D-028.
	IssueKey(ctx context.Context, key Key) error
	// RevokeKey revokes one live key and returns its project (ErrNotFound: unknown or already revoked).
	RevokeKey(ctx context.Context, id uuid.UUID) (project uuid.UUID, err error)
	// RotateKey revokes old and stores next for the same project in one step (one transaction in
	// Postgres). next.Label "" keeps old's label. Returns the project and the label stored.
	RotateKey(ctx context.Context, old uuid.UUID, next Key) (project uuid.UUID, label string, err error)
	// SetSuspended suspends or resumes a project (D-027); returns suspended_at (zero on resume).
	SetSuspended(ctx context.Context, id uuid.UUID, suspended bool) (time.Time, error)
	// ListSuspended is every suspended project (boot load, reconcile).
	ListSuspended(ctx context.Context) ([]uuid.UUID, error)
	// ListProjects is one keyset page on (created_at, id) after the given position (zero = first).
	ListProjects(ctx context.Context, afterCreated time.Time, afterID uuid.UUID, limit int) ([]Project, error)
	// GetProject is one project and every key of it (revoked included), never a hash.
	GetProject(ctx context.Context, id uuid.UUID) (Project, []Key, error)
	// TouchKey records a use of the key (last_used_at; the caller throttles, D-028).
	TouchKey(ctx context.Context, id uuid.UUID) error
}

// storeNow is a creation time with the precision Postgres keeps, so a cursor round-trips the same
// in both stores.
func storeNow() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }

// MemStore keeps projects in memory: persistence off. Lost on restart, and not replicated: a
// replica's MemStore is empty.
type MemStore struct {
	mu       sync.RWMutex
	names    map[string]uuid.UUID
	projects map[uuid.UUID]*Project // Keys is computed on read
	keys     map[uuid.UUID]*Key     // by key id, revoked included
	byHash   map[string]uuid.UUID   // string(hash) → key id
}

// NewMemStore returns an empty in-memory store.
func NewMemStore() *MemStore {
	return &MemStore{
		names: map[string]uuid.UUID{}, projects: map[uuid.UUID]*Project{},
		keys: map[uuid.UUID]*Key{}, byHash: map[string]uuid.UUID{},
	}
}

// addKey stores k (caller holds mu).
func (m *MemStore) addKey(k Key) {
	if k.CreatedAt.IsZero() {
		k.CreatedAt = storeNow()
	}
	m.keys[k.ID] = &k
	m.byHash[string(k.Hash)] = k.ID
}

// live returns the live key with this hash (caller holds mu).
func (m *MemStore) live(hash []byte) (*Key, bool) {
	k, ok := m.keys[m.byHash[string(hash)]]
	if !ok || !k.RevokedAt.IsZero() {
		return nil, false
	}
	return k, true
}

func (m *MemStore) CreateProject(_ context.Context, id uuid.UUID, name string, key Key) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.names[name]; ok {
		return fmt.Errorf("api: create project %q: %w", name, ErrConflict)
	}
	m.names[name] = id
	m.projects[id] = &Project{ID: id, Name: name, CreatedAt: storeNow()}
	key.ProjectID = id
	m.addKey(key)
	return nil
}

func (m *MemStore) ProjectExists(_ context.Context, id uuid.UUID) (bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.projects[id]
	return ok, nil
}

func (m *MemStore) ProjectByKeyHash(_ context.Context, hash []byte) (uuid.UUID, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	k, ok := m.live(hash)
	if !ok {
		return uuid.Nil, ErrNotFound
	}
	return k.ProjectID, nil
}

// ResolveKey is tenant.KeyResolver: the live key with this hash and its project.
func (m *MemStore) ResolveKey(_ context.Context, hash []byte) (uuid.UUID, uuid.UUID, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	k, ok := m.live(hash)
	if !ok {
		return uuid.Nil, uuid.Nil, tenant.ErrUnknownKey
	}
	return k.ID, k.ProjectID, nil
}

func (m *MemStore) IssueKey(_ context.Context, key Key) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.projects[key.ProjectID]; !ok {
		return ErrNotFound
	}
	m.addKey(key)
	return nil
}

func (m *MemStore) RevokeKey(_ context.Context, id uuid.UUID) (uuid.UUID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k, ok := m.keys[id]
	if !ok || !k.RevokedAt.IsZero() {
		return uuid.Nil, ErrNotFound
	}
	k.RevokedAt = storeNow()
	return k.ProjectID, nil
}

func (m *MemStore) RotateKey(_ context.Context, old uuid.UUID, next Key) (uuid.UUID, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k, ok := m.keys[old]
	if !ok || !k.RevokedAt.IsZero() {
		return uuid.Nil, "", ErrNotFound
	}
	k.RevokedAt = storeNow()
	next.ProjectID = k.ProjectID
	if next.Label == "" {
		next.Label = k.Label
	}
	m.addKey(next)
	return k.ProjectID, next.Label, nil
}

func (m *MemStore) SetSuspended(_ context.Context, id uuid.UUID, suspended bool) (time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.projects[id]
	if !ok {
		return time.Time{}, ErrNotFound
	}
	switch {
	case !suspended:
		p.SuspendedAt = time.Time{}
	case p.SuspendedAt.IsZero():
		p.SuspendedAt = storeNow()
	}
	return p.SuspendedAt, nil
}

func (m *MemStore) ListSuspended(context.Context) ([]uuid.UUID, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []uuid.UUID
	for id, p := range m.projects {
		if !p.SuspendedAt.IsZero() {
			out = append(out, id)
		}
	}
	return out, nil
}

// liveKeys counts project's live keys (caller holds mu).
func (m *MemStore) liveKeys(project uuid.UUID) int {
	n := 0
	for _, k := range m.keys {
		if k.ProjectID == project && k.RevokedAt.IsZero() {
			n++
		}
	}
	return n
}

func (m *MemStore) ListProjects(_ context.Context, afterCreated time.Time, afterID uuid.UUID, limit int) ([]Project, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Project, 0, len(m.projects))
	for _, p := range m.projects {
		if c := p.CreatedAt.Compare(afterCreated); c > 0 || (c == 0 && slices.Compare(p.ID[:], afterID[:]) > 0) {
			cp := *p
			cp.Keys = m.liveKeys(p.ID)
			out = append(out, cp)
		}
	}
	slices.SortFunc(out, func(a, b Project) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return slices.Compare(a.ID[:], b.ID[:])
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *MemStore) GetProject(_ context.Context, id uuid.UUID) (Project, []Key, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.projects[id]
	if !ok {
		return Project{}, nil, ErrNotFound
	}
	out := *p
	out.Keys = m.liveKeys(id)
	var keys []Key
	for _, k := range m.keys {
		if k.ProjectID == id {
			cp := *k
			cp.Hash = nil // never leaves the store
			keys = append(keys, cp)
		}
	}
	slices.SortFunc(keys, func(a, b Key) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return slices.Compare(a.ID[:], b.ID[:])
	})
	return out, keys, nil
}

func (m *MemStore) TouchKey(_ context.Context, id uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if k, ok := m.keys[id]; ok {
		k.LastUsedAt = storeNow()
	}
	return nil
}

// DBStore keeps projects in PostgreSQL (db/query/registry).
type DBStore struct{ store *db.Store }

// NewDBStore wraps the process's db.Store.
func NewDBStore(store *db.Store) *DBStore { return &DBStore{store: store} }

func (d *DBStore) CreateProject(ctx context.Context, id uuid.UUID, name string, key Key) error {
	return d.store.ExecTx(ctx, func(tx *db.Store) error {
		if _, err := tx.Registry.CreateProject(ctx, registry.CreateProjectParams{ID: id, Name: name}); err != nil {
			if isUniqueViolation(err) {
				return fmt.Errorf("api: create project %q: %w", name, ErrConflict)
			}
			return fmt.Errorf("api: create project %q: %w", name, err)
		}
		if _, err := tx.Registry.CreateAPIKey(ctx, registry.CreateAPIKeyParams{
			ID: key.ID, ProjectID: id, KeyHash: key.Hash, KeyPrefix: key.Prefix, Label: key.Label,
		}); err != nil {
			return fmt.Errorf("api: create key for %q: %w", name, err)
		}
		return nil
	})
}

func (d *DBStore) ProjectExists(ctx context.Context, id uuid.UUID) (bool, error) {
	if _, err := d.store.Registry.GetProject(ctx, id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("api: get project: %w", err)
	}
	return true, nil
}

func (d *DBStore) ProjectByKeyHash(ctx context.Context, hash []byte) (uuid.UUID, error) {
	row, err := d.store.Registry.GetActiveAPIKeyByHash(ctx, hash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, ErrNotFound
		}
		return uuid.Nil, fmt.Errorf("api: key lookup: %w", err)
	}
	return row.ProjectID, nil
}

// ResolveKey is tenant.KeyResolver: the live (unrevoked) key with this hash and its project.
func (d *DBStore) ResolveKey(ctx context.Context, hash []byte) (uuid.UUID, uuid.UUID, error) {
	row, err := d.store.Registry.GetActiveAPIKeyByHash(ctx, hash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, uuid.Nil, tenant.ErrUnknownKey
		}
		return uuid.Nil, uuid.Nil, fmt.Errorf("api: key lookup: %w", err)
	}
	return row.ID, row.ProjectID, nil
}

// notFound maps pgx.ErrNoRows to ErrNotFound and wraps everything else with what.
func notFound(err error, what string) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return fmt.Errorf("api: %s: %w", what, err)
}

func (d *DBStore) IssueKey(ctx context.Context, key Key) error {
	return d.store.ExecTx(ctx, func(tx *db.Store) error {
		if _, err := tx.Registry.GetProject(ctx, key.ProjectID); err != nil {
			return notFound(err, "issue key: get project")
		}
		if _, err := tx.Registry.CreateAPIKey(ctx, registry.CreateAPIKeyParams{
			ID: key.ID, ProjectID: key.ProjectID, KeyHash: key.Hash, KeyPrefix: key.Prefix, Label: key.Label,
		}); err != nil {
			return fmt.Errorf("api: issue key: %w", err)
		}
		return nil
	})
}

func (d *DBStore) RevokeKey(ctx context.Context, id uuid.UUID) (uuid.UUID, error) {
	project, err := d.store.Registry.RevokeAPIKey(ctx, id)
	if err != nil {
		return uuid.Nil, notFound(err, "revoke key")
	}
	return project, nil
}

func (d *DBStore) RotateKey(ctx context.Context, old uuid.UUID, next Key) (uuid.UUID, string, error) {
	var project uuid.UUID
	label := next.Label
	err := d.store.ExecTx(ctx, func(tx *db.Store) error {
		row, err := tx.Registry.GetAPIKey(ctx, old)
		if err != nil {
			return notFound(err, "rotate: get key")
		}
		if row.RevokedAt.Valid {
			return ErrNotFound
		}
		if project, err = tx.Registry.RevokeAPIKey(ctx, old); err != nil {
			return notFound(err, "rotate: revoke")
		}
		if label == "" {
			label = row.Label
		}
		if _, err := tx.Registry.CreateAPIKey(ctx, registry.CreateAPIKeyParams{
			ID: next.ID, ProjectID: project, KeyHash: next.Hash, KeyPrefix: next.Prefix, Label: label,
		}); err != nil {
			return fmt.Errorf("api: rotate: create key: %w", err)
		}
		return nil
	})
	if err != nil {
		return uuid.Nil, "", err
	}
	return project, label, nil
}

func (d *DBStore) SetSuspended(ctx context.Context, id uuid.UUID, suspended bool) (time.Time, error) {
	at, err := d.store.Registry.SetProjectSuspended(ctx, registry.SetProjectSuspendedParams{Suspended: suspended, ID: id})
	if err != nil {
		return time.Time{}, notFound(err, "set suspended")
	}
	return tsTime(at), nil
}

func (d *DBStore) ListSuspended(ctx context.Context) ([]uuid.UUID, error) {
	ids, err := d.store.Registry.ListSuspendedProjects(ctx)
	if err != nil {
		return nil, fmt.Errorf("api: list suspended: %w", err)
	}
	return ids, nil
}

func (d *DBStore) ListProjects(ctx context.Context, afterCreated time.Time, afterID uuid.UUID, limit int) ([]Project, error) {
	rows, err := d.store.Registry.ListProjectsPage(ctx, registry.ListProjectsPageParams{
		AfterCreatedAt: afterCreated, AfterID: afterID, PageLimit: int32(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("api: list projects: %w", err)
	}
	out := make([]Project, len(rows))
	for i, r := range rows {
		out[i] = Project{ID: r.ID, Name: r.Name, CreatedAt: r.CreatedAt, SuspendedAt: tsTime(r.SuspendedAt), Keys: int(r.Keys)}
	}
	return out, nil
}

func (d *DBStore) GetProject(ctx context.Context, id uuid.UUID) (Project, []Key, error) {
	p, err := d.store.Registry.GetProject(ctx, id)
	if err != nil {
		return Project{}, nil, notFound(err, "get project")
	}
	rows, err := d.store.Registry.ListProjectKeys(ctx, id)
	if err != nil {
		return Project{}, nil, fmt.Errorf("api: list keys: %w", err)
	}
	out := Project{ID: p.ID, Name: p.Name, CreatedAt: p.CreatedAt, SuspendedAt: tsTime(p.SuspendedAt)}
	keys := make([]Key, len(rows))
	for i, r := range rows {
		keys[i] = Key{
			ID: r.ID, ProjectID: id, Prefix: r.KeyPrefix, Label: r.Label, CreatedAt: r.CreatedAt,
			RevokedAt: tsTime(r.RevokedAt), LastUsedAt: tsTime(r.LastUsedAt),
		}
		if !r.RevokedAt.Valid {
			out.Keys++
		}
	}
	return out, keys, nil
}

func (d *DBStore) TouchKey(ctx context.Context, id uuid.UUID) error {
	if err := d.store.Registry.TouchAPIKeyUsed(ctx, id); err != nil {
		return fmt.Errorf("api: touch key: %w", err)
	}
	return nil
}

// tsTime is a nullable timestamptz as a time (zero when NULL).
func tsTime(t pgtype.Timestamptz) time.Time {
	if !t.Valid {
		return time.Time{}
	}
	return t.Time
}

// isUniqueViolation reports a Postgres unique_violation (SQLSTATE 23505).
func isUniqueViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	return errors.As(err, &pgErr) && pgErr.SQLState() == "23505"
}
