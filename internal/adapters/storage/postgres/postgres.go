// Package postgres implements ports.OrderStore and ports.SchedulerStore on PostgreSQL.
//
// It is the durable multi-process store: order updates serialize on a row lock
// (SELECT … FOR UPDATE) and scheduler eligibility claims use FOR UPDATE SKIP LOCKED.
// Blobs deliberately stay on the local adapter in C5, so this package exposes no
// BlobStore. Provider concepts (SQL, pgx types, leases) never cross the port boundary.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/artemidius/insteadly/internal/core/ports"
)

const (
	connectTimeout = 10 * time.Second
	defaultMaxConn = 10
)

// Config configures one Postgres adapter. DSN is the full libpq/URL connection string.
type Config struct {
	DSN    string
	Logger *slog.Logger
}

// Store is the Postgres order + scheduler store. It owns one pgxpool.
type Store struct {
	pool *pgxpool.Pool
	log  *slog.Logger
}

var (
	_ ports.OrderStore     = (*Store)(nil)
	_ ports.SchedulerStore = (*Store)(nil)
)

// New parses the DSN, opens the pool, and pings once so a bad configuration fails at
// startup rather than on the first request. It performs no migration.
func New(ctx context.Context, cfg Config) (*Store, error) {
	if strings.TrimSpace(cfg.DSN) == "" {
		return nil, errors.New("postgres: dsn is required")
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	poolCfg, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse dsn: %w", err)
	}
	if poolCfg.MaxConns < 1 {
		poolCfg.MaxConns = defaultMaxConn
	}
	// All timestamptz values round-trip as UTC; eligibility comparisons are instant
	// based, so this only makes stored/selected times predictable.
	if poolCfg.ConnConfig.RuntimeParams == nil {
		poolCfg.ConnConfig.RuntimeParams = make(map[string]string)
	}
	poolCfg.ConnConfig.RuntimeParams["timezone"] = "UTC"

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("postgres: open pool: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: ping: %w", err)
	}
	return &Store{pool: pool, log: log}, nil
}

// Orders returns the order store (the Store itself).
func (s *Store) Orders() ports.OrderStore { return s }

// Scheduler returns the scheduler store (the Store itself).
func (s *Store) Scheduler() ports.SchedulerStore { return s }

// Close releases the pool. It is safe to call more than once.
func (s *Store) Close() error {
	if s == nil || s.pool == nil {
		return nil
	}
	s.pool.Close()
	return nil
}
