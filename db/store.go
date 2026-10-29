// Package db is the cache database: the pgx pool, the generated query packages under db/sqlc, and
// the embedded golang-migrate files under db/migration (D-015). One Store per process, created in
// cmd/redlite from config and passed in; nothing else opens a connection.
package db

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5" // registers the pgx5:// driver
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/CrimsonCoder42/red_lite_cache/db/sqlc/cluster"
	"github.com/CrimsonCoder42/red_lite_cache/db/sqlc/entries"
	"github.com/CrimsonCoder42/red_lite_cache/db/sqlc/registry"
)

//go:embed migration/*.sql
var migrations embed.FS

// Store wraps the pool and the generated queriers, with ExecTx for one transaction across them.
// after backendAdvo/db/sqlc/store.go: same Store + ExecTx shape, three query packages instead of
// sixteen, and the store lives beside db/sqlc so the generated folder stays generated-only.
type Store struct {
	pool *pgxpool.Pool

	Registry registry.Querier
	Entries  entries.Querier
	Cluster  cluster.Querier
}

// NewStore builds a Store on pool.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{
		pool:     pool,
		Registry: registry.New(pool),
		Entries:  entries.New(pool),
		Cluster:  cluster.New(pool),
	}
}

// Pool returns the underlying pool (health checks, Close).
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// ExecTx runs fn with a Store whose queriers share one transaction; it commits when fn returns nil
// and rolls back otherwise.
func (s *Store) ExecTx(ctx context.Context, fn func(*Store) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("db: begin: %w", err)
	}
	defer tx.Rollback(ctx) // a no-op after Commit
	txStore := &Store{
		pool:     s.pool,
		Registry: registry.New(tx),
		Entries:  entries.New(tx),
		Cluster:  cluster.New(tx),
	}
	if err := fn(txStore); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("db: commit: %w", err)
	}
	return nil
}

// Batch sends b on one pooled connection inside a transaction and checks every queued statement.
// The persist writer uses it to flush a batch in one round trip.
func (s *Store) Batch(ctx context.Context, b *pgx.Batch) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		br := tx.SendBatch(ctx, b)
		for range b.Len() {
			if _, err := br.Exec(); err != nil {
				br.Close()
				return fmt.Errorf("db: batch: %w", err)
			}
		}
		return br.Close()
	})
}

// OpenPool creates the pgx pool from a postgres:// URL with the configured bounds and pings it.
func OpenPool(ctx context.Context, databaseURL string, minConns, maxConns int32) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("db: parse url: %w", err)
	}
	cfg.MinConns = minConns
	cfg.MaxConns = maxConns
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("db: open pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: ping: %w", err)
	}
	return pool, nil
}

// Migrate applies every embedded migration to databaseURL (postgres:// or postgresql://). A
// database already at the latest version is not an error.
func Migrate(databaseURL string) error {
	src, err := iofs.New(migrations, "migration")
	if err != nil {
		return fmt.Errorf("db: migration source: %w", err)
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, migrateURL(databaseURL))
	if err != nil {
		return fmt.Errorf("db: migrate init: %w", err)
	}
	defer m.Close()
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("db: migrate up: %w", err)
	}
	return nil
}

// migrateURL rewrites the scheme to the one golang-migrate's pgx v5 driver registers.
func migrateURL(databaseURL string) string {
	for _, scheme := range []string{"postgresql://", "postgres://"} {
		if rest, ok := strings.CutPrefix(databaseURL, scheme); ok {
			return "pgx5://" + rest
		}
	}
	return databaseURL
}
