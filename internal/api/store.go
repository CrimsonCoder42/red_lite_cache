package api

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/CrimsonCoder42/red_lite_cache/db"
	"github.com/CrimsonCoder42/red_lite_cache/db/sqlc/registry"
)

// ErrNotFound is returned when a project or an API key does not exist (or the key is revoked).
var ErrNotFound = errors.New("api: not found")

// ErrConflict is returned when a project name is taken.
var ErrConflict = errors.New("api: conflict")

// Key is one issued API key as stored: never the key itself, only its hash and prefix.
type Key struct {
	ID        uuid.UUID
	ProjectID uuid.UUID
	Hash      []byte // sha256 of the key
	Prefix    string // first 8 characters, for logs
}

// Store holds projects and their API keys.
type Store interface {
	// CreateProject stores the project and its first key in one step.
	CreateProject(ctx context.Context, id uuid.UUID, name string, key Key) error
	// ProjectExists reports whether id is a project.
	ProjectExists(ctx context.Context, id uuid.UUID) (bool, error)
	// ProjectByKeyHash resolves a presented key's hash to its project (ErrNotFound when unknown).
	ProjectByKeyHash(ctx context.Context, hash []byte) (uuid.UUID, error)
}

// MemStore keeps projects in memory: persistence off. Lost on restart.
type MemStore struct {
	mu       sync.RWMutex
	names    map[string]uuid.UUID
	projects map[uuid.UUID]struct{}
	keys     map[string]uuid.UUID // string(hash) → project
}

// NewMemStore returns an empty in-memory store.
func NewMemStore() *MemStore {
	return &MemStore{names: map[string]uuid.UUID{}, projects: map[uuid.UUID]struct{}{}, keys: map[string]uuid.UUID{}}
}

func (m *MemStore) CreateProject(_ context.Context, id uuid.UUID, name string, key Key) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.names[name]; ok {
		return fmt.Errorf("api: create project %q: %w", name, ErrConflict)
	}
	m.names[name] = id
	m.projects[id] = struct{}{}
	m.keys[string(key.Hash)] = id
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
	id, ok := m.keys[string(hash)]
	if !ok {
		return uuid.Nil, ErrNotFound
	}
	return id, nil
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
			ID: key.ID, ProjectID: id, KeyHash: key.Hash, KeyPrefix: key.Prefix,
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

// isUniqueViolation reports a Postgres unique_violation (SQLSTATE 23505).
func isUniqueViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	return errors.As(err, &pgErr) && pgErr.SQLState() == "23505"
}
