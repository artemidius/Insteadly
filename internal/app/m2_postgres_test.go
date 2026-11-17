//go:build postgres

package app_test

import (
	"bytes"
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
	"github.com/artemidius/insteadly/internal/app"
	"github.com/artemidius/insteadly/internal/core/config"
	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/core/runner"
	"github.com/artemidius/insteadly/internal/core/scheduler"
)

var m2PostgresDSN string

// TestMain starts one Postgres container for the tagged app tests. Without the tag this
// file (and therefore this TestMain) does not exist, so the default suite stays
// Docker-free.
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
	m2PostgresDSN = dsn
	code := m.Run()
	if err := testcontainers.TerminateContainer(container); err != nil {
		fmt.Fprintf(os.Stderr, "terminate postgres container: %v\n", err)
	}
	os.Exit(code)
}

func newM2PostgresStore(t *testing.T) *pgstore.Store {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := pgstore.New(ctx, pgstore.Config{DSN: m2PostgresDSN})
	if err != nil {
		t.Fatalf("pgstore.New: %v", err)
	}
	if err := store.Migrate(ctx); err != nil {
		_ = store.Close()
		t.Fatalf("Migrate: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	conn, err := pgx.Connect(ctx, m2PostgresDSN)
	if err != nil {
		t.Fatalf("pgx.Connect: %v", err)
	}
	if _, err := conn.Exec(ctx, `TRUNCATE orders`); err != nil {
		_ = conn.Close(ctx)
		t.Fatalf("truncate: %v", err)
	}
	if err := conn.Close(ctx); err != nil {
		t.Fatalf("close admin conn: %v", err)
	}
	return store
}

func TestM2PostgresCompositionAndLocalBlobs(t *testing.T) {
	cfg := m2TestConfig()
	cfg.Storage = config.Storage{
		Provider: "postgres",
		Local:    config.Local{Dir: t.TempDir()},
		Postgres: config.Postgres{DSN: m2PostgresDSN},
	}
	cfg.Scheduler.Enabled = false
	a, err := app.New(cfg)
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	if a.Storage == nil || a.Scheduler == nil || a.Events == nil {
		t.Fatal("postgres app is not fully wired")
	}
	ctx := context.Background()
	order, err := a.Storage.Orders().Create(ctx, domain.Order{
		Clinic:  domain.Clinic{Name: "Praxis PG", Phone: "+431234567"},
		Patient: domain.Patient{FullName: "Max"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := a.Storage.Orders().Get(ctx, order.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != order.ID {
		t.Errorf("order = %+v", got)
	}

	// Blobs must stay on the local adapter even when orders are on Postgres.
	key := domain.BlobKey{OrderID: order.ID, Kind: domain.BlobKindRecording, Name: "call.wav"}
	if err := a.Storage.Blobs().Put(ctx, key, bytes.NewReader([]byte("pcm"))); err != nil {
		t.Fatalf("blob Put: %v", err)
	}
	rc, err := a.Storage.Blobs().Open(ctx, key)
	if err != nil {
		t.Fatalf("blob Open: %v", err)
	}
	_ = rc.Close()

	closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.Close(closeCtx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := a.CloseStorage(); err != nil {
		t.Fatalf("CloseStorage: %v", err)
	}
}

func TestM2PostgresSchedulerBusyRetry(t *testing.T) {
	store := newM2PostgresStore(t)
	clock := scheduler.NewFakeClock(time.Date(2026, 1, 5, 9, 0, 0, 0, time.UTC))
	telephony := &m2Telephony{scripts: []m2Script{{busy: true}, {answer: true}}}
	booked := domain.Outcome{
		Kind:       domain.OutcomeBooked,
		BookedSlot: &domain.BookedSlot{DateTime: clock.Now().Add(24 * time.Hour)},
	}
	agent := &m2Agent{outcomes: []domain.Outcome{booked}}
	run, err := runner.New(runner.Config{
		Telephony:      telephony,
		Orders:         store.Orders(),
		Agent:          agent,
		CallTimeout:    5 * time.Second,
		CleanupTimeout: 500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	windows, err := scheduler.ParseWindows(config.DefaultSchedulerWindows)
	if err != nil {
		t.Fatalf("ParseWindows: %v", err)
	}
	sched, err := scheduler.New(scheduler.Config{
		Store:           store,
		Orders:          store.Orders(),
		Runner:          run,
		Clock:           clock,
		Windows:         windows,
		Tick:            time.Hour,
		MaxAttempts:     3,
		MaxConcurrent:   2,
		Lease:           time.Minute,
		BusyBackoff:     15 * time.Minute,
		NoAnswerBackoff: 30 * time.Minute,
		ErrorBackoff:    5 * time.Minute,
		WorkerID:        "m2-postgres",
	})
	if err != nil {
		t.Fatalf("scheduler.New: %v", err)
	}

	ctx := context.Background()
	created, err := store.Create(ctx, domain.Order{
		Clinic:  domain.Clinic{Name: "Praxis PG", Phone: "+431234567"},
		Patient: domain.Patient{FullName: "Max"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := store.Update(ctx, created.ID, func(o *domain.Order) error {
		return o.Transition(domain.OrderPaid)
	}); err != nil {
		t.Fatalf("mark paid: %v", err)
	}

	runOnce := func() {
		t.Helper()
		if err := sched.RunOnce(ctx); err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
		waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := sched.Wait(waitCtx); err != nil {
			t.Fatalf("Wait: %v", err)
		}
	}
	runOnce()

	requeued, err := store.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if requeued.Status != domain.OrderScheduled || requeued.Outcome != nil || len(requeued.AttemptHistory) != 1 {
		t.Fatalf("requeued = %+v", requeued)
	}
	wantNext := clock.Now().Add(15 * time.Minute)
	if requeued.NextAttemptAt == nil || !requeued.NextAttemptAt.Equal(wantNext) {
		t.Fatalf("next_attempt_at = %v, want %v", requeued.NextAttemptAt, wantNext)
	}
	if requeued.AttemptHistory[0].Outcome.FailReason != "busy" {
		t.Errorf("history = %+v", requeued.AttemptHistory)
	}

	clock.Advance(16 * time.Minute)
	runOnce()
	final, err := store.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if final.Status != domain.OrderFinished || final.Outcome == nil || final.Outcome.Kind != domain.OutcomeBooked {
		t.Fatalf("final = %+v", final)
	}
	if final.Attempts != 2 {
		t.Errorf("attempts = %d, want 2", final.Attempts)
	}
	if telephony.dialCount() != 2 || agent.runCount() != 1 {
		t.Errorf("dials = %d, agent runs = %d, want 2/1", telephony.dialCount(), agent.runCount())
	}

	closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := sched.Close(closeCtx); err != nil {
		t.Fatalf("scheduler.Close: %v", err)
	}
}
