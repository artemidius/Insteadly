//go:build postgres

package postgres_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	pgstore "github.com/artemidius/insteadly/internal/adapters/storage/postgres"
)

var testDSN string

// TestMain starts one ephemeral Postgres container for the package. These tests are
// gated by //go:build postgres: the default `go test ./...` and `make verify` never
// require Docker or this module.
func TestMain(m *testing.M) {
	ctx := context.Background()
	container, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("insteadly"),
		tcpostgres.WithUsername("insteadly"),
		tcpostgres.WithPassword("insteadly"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(90*time.Second),
		),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "postgres testcontainer: %v\n", err)
		os.Exit(1)
	}
	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		fmt.Fprintf(os.Stderr, "postgres connection string: %v\n", err)
		_ = testcontainers.TerminateContainer(container)
		os.Exit(1)
	}
	testDSN = dsn
	code := m.Run()
	if err := testcontainers.TerminateContainer(container); err != nil {
		fmt.Fprintf(os.Stderr, "terminate postgres container: %v\n", err)
	}
	os.Exit(code)
}

// newTestStore opens a migrated store against the shared container and empties the
// orders table so every test starts from a known state.
func newTestStore(t *testing.T) *pgstore.Store {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := pgstore.New(ctx, pgstore.Config{DSN: testDSN})
	if err != nil {
		t.Fatalf("postgres New: %v", err)
	}
	if err := store.Migrate(ctx); err != nil {
		store.Close()
		t.Fatalf("Migrate: %v", err)
	}
	conn, err := pgx.Connect(ctx, testDSN)
	if err != nil {
		store.Close()
		t.Fatalf("pgx.Connect: %v", err)
	}
	if _, err := conn.Exec(ctx, `TRUNCATE orders`); err != nil {
		_ = conn.Close(ctx)
		store.Close()
		t.Fatalf("truncate orders: %v", err)
	}
	if err := conn.Close(ctx); err != nil {
		t.Fatalf("close admin conn: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}
